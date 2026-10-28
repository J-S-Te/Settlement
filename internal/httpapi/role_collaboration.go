package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
)

// serveRoleCollaboration 用途：路由责任事件、项目协作与人工计划接口。
// 参数：w为响应，r为经总入口许可和CSRF检查的请求。
// 返回值：已处理本组路由时真，未匹配时假。
func (a *API) serveRoleCollaboration(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.URL.Path == "/api/v1/project-candidates" && r.Method == http.MethodGet:
		a.projectCandidates(w, r)
	case r.URL.Path == "/api/v1/project-assistants" && r.Method == http.MethodGet:
		a.projectAssistants(w, r)
	case r.URL.Path == "/internal/v1/settlement/events/contract-responsibility" && r.Method == http.MethodPost:
		a.contractResponsibility(w, r)
	case r.URL.Path == "/api/v1/contract-references" && r.Method == http.MethodGet:
		a.contractReferences(w, r)
	case r.URL.Path == "/api/v1/project-assignments" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		a.projectAssignments(w, r)
	case r.URL.Path == "/api/v1/project-mappings" && r.Method == http.MethodPut:
		a.projectMappingNote(w, r)
	case r.URL.Path == "/api/v1/receivable-plans" && r.Method == http.MethodPost:
		a.createManualPlan(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/receivable-plans/") && strings.HasSuffix(r.URL.Path, "/adjust") && r.Method == http.MethodPost:
		a.adjustPlan(w, r)
	default:
		return false
	}
	return true
}

// projectCandidates 用途：从权威项目服务查询待分配项目，避免手填映射冒充存在。
// 参数：w为响应，r包含project_id精确查询参数。
// 返回值：无，返回已验证的项目及其合同客户引用。
func (a *API) projectCandidates(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.collaboration.manage")
	if !ok {
		return
	}
	if !p.Unrestricted("settlement.collaboration.manage") {
		respondError(w, service.ErrForbidden)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if id == "" || len(id) > 128 {
		respondError(w, service.ErrInvalid)
		return
	}
	if a.service.ProjectReferences == nil {
		respondError(w, service.ErrDependency)
		return
	}
	ref, err := a.service.ProjectReferences.Resolve(r.Context(), p.TenantID, id)
	if err != nil {
		respondError(w, service.ErrDependency)
		return
	}
	if !ref.Active {
		respondError(w, service.ErrNotFound)
		return
	}
	write(w, http.StatusOK, []map[string]any{{"project_id": ref.ProjectID, "project_name": ref.Name, "contract_id": ref.ContractID, "customer_id": ref.CustomerID, "version": ref.Version}})
}

// projectAssistants 用途：提供平台有效项目助理候选，不能任意账号分配。
// 参数：w为响应，r包含人员搜索关键词。
// 返回值：无，返回当前有效人员及总数，依赖失败关闭。
func (a *API) projectAssistants(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.user(w, r, "settlement.collaboration.manage"); !ok {
		return
	}
	directory, ok := a.directory.(interface {
		ListRole(context.Context, string, string, int, int) ([]platform.Personnel, int64, error)
	})
	if !ok {
		respondError(w, service.ErrDependency)
		return
	}
	items, total, err := directory.ListRole(r.Context(), "settlement_project_assistant", r.URL.Query().Get("keyword"), 1, 100)
	if err != nil {
		respondError(w, service.ErrDependency)
		return
	}
	write(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// contractResponsibility 用途：接收合同权威端的独立责任变更。
// 参数：w为响应，r为包含机器Bearer及责任事件的请求。
// 返回值：无，接收结果通过HTTP返回。
func (a *API) contractResponsibility(w http.ResponseWriter, r *http.Request) {
	tenant, ok := a.integration(r)
	if !ok || tenant == "" {
		fail(w, http.StatusUnauthorized, "SETTLEMENT_MACHINE_UNAUTHENTICATED", "服务身份或租户无效")
		return
	}
	var event service.ContractResponsibilityEvent
	if !decode(w, r, &event) {
		return
	}
	changed, err := a.service.IngestContractResponsibility(r.Context(), tenant, event)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusAccepted, map[string]any{"accepted": true, "event_id": event.EventID, "applied": changed})
}

// contractReferences 用途：返回当前责任范围内的合同选择项。
// 参数：w为响应，r为包含分页参数的当前会话请求。
// 返回值：无，仅返回可见合同，不接受客户端扩大范围。
func (a *API) contractReferences(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receivable.read")
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		respondError(w, service.ErrInvalid)
		return
	}
	rows, err := a.queryScoped(r.Context(), p, "settlement.receivable.read", `SELECT cs.id,cs.source_contract_no,cs.customer_id,cs.customer_name_snapshot,cs.project_id,cs.currency,cs.contract_amount,COALESCE(cr.owner_user_id,'') FROM settlement_contract_snapshot cs LEFT JOIN settlement_contract_responsibility cr ON cr.tenant_id=cs.tenant_id AND cr.source_contract_id=cs.source_contract_id AND cr.active=TRUE WHERE cs.tenant_id=? AND cs.financial_status='EFFECTIVE' ORDER BY cs.updated_at DESC,cs.id LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, customer, name, project, currency, amount, owner string
		if err = rows.Scan(&id, &no, &customer, &name, &project, &currency, &amount, &owner); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "contract_no": no, "customer_id": customer, "customer_name": name, "project_id": project, "currency": currency, "contract_amount": amount, "owner_user_id": owner})
	}
	if err = rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

// projectAssignments 用途：管理项目协作分配或读取个人负责项目。
// 参数：w为响应，r为GET分页或POST版本化分配请求。
// 返回值：无，返回分配结果或可见协作列表。
func (a *API) projectAssignments(w http.ResponseWriter, r *http.Request) {
	permission := "settlement.collaboration.read"
	if r.Method == http.MethodPost {
		permission = "settlement.collaboration.manage"
	}
	p, ok := a.user(w, r, permission)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		var in service.ProjectAssignmentInput
		if !decode(w, r, &in) {
			return
		}
		if err := a.service.AssignProject(r.Context(), p, in); err != nil {
			respondError(w, err)
			return
		}
		write(w, http.StatusOK, map[string]any{"saved": true})
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		respondError(w, service.ErrInvalid)
		return
	}
	where := "pa.tenant_id=?"
	args := []any{p.TenantID}
	if !p.Unrestricted("settlement.collaboration.read") {
		where += " AND pa.user_id=?"
		args = append(args, p.UserID)
	}
	args = append(args, limit, offset)
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT pa.project_id,pr.project_name,pr.source_contract_id,pa.user_id,pa.version,pa.active,pa.note FROM settlement_project_assignment pa JOIN settlement_project_reference pr ON pr.tenant_id=pa.tenant_id AND pr.project_id=pa.project_id WHERE `+where+` ORDER BY pa.updated_at DESC,pa.project_id,pa.user_id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var project, name, contract, user, note string
		var version uint64
		var active bool
		if err = rows.Scan(&project, &name, &contract, &user, &version, &active, &note); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"project_id": project, "project_name": name, "contract_id": contract, "user_id": user, "version": version, "active": active, "note": note})
	}
	if err = rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

// projectMappingNote 用途：允许助理维护已分配项目的映射说明，不能变更权威归属。
// 参数：w为响应，r为项目、版本和说明请求。
// 返回值：无，说明修改留痕并返回结果。
func (a *API) projectMappingNote(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.mapping.manage")
	if !ok {
		return
	}
	var in struct {
		ProjectID string `json:"project_id"`
		Version   uint64 `json:"version"`
		Note      string `json:"note"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := a.service.UpdateProjectMappingNote(r.Context(), p, in.ProjectID, in.Version, in.Note); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]bool{"saved": true})
}

// createManualPlan 用途：建立绑定有效合同的待确认人工收款计划。
// 参数：w为响应，r为包含幂等键及计划字段的请求。
// 返回值：无，返回新计划ID或真实错误。
func (a *API) createManualPlan(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receivable.create")
	if !ok {
		return
	}
	var in service.ManualPlanInput
	if !decode(w, r, &in) {
		return
	}
	id, err := a.service.CreateManualPlan(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": id, "status": "PENDING_CONFIRMATION"})
}

// adjustPlan 用途：对已确认计划执行有原因、版本及金额下限约束的调整。
// 参数：w为响应，r为计划ID与调整字段请求。
// 返回值：无，成功后返回保存状态。
func (a *API) adjustPlan(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receivable.adjust")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/receivable-plans/"), "/adjust")
	var in service.PlanConfirmationInput
	if !decode(w, r, &in) {
		return
	}
	if err := a.service.AdjustPlan(r.Context(), p, id, in); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]bool{"saved": true})
}
