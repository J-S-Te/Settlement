package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/j-s-te/settlement/internal/service"
)

// serveRecognitions 提供财务登记、提交、独立复核及最小候选选择业务入口。
// 参数：w/r为已经过公共许可与CSRF边界的请求响应。
// 返回值：属于此组路由返回真，所有业务动作仍复核明确组织范围。
func (a *API) serveRecognitions(w http.ResponseWriter, r *http.Request) bool {
	const base = "/api/v1/recognitions"
	if r.URL.Path != base && !strings.HasPrefix(r.URL.Path, base+"/") {
		return false
	}
	permission := "settlement.recognition.read"
	if r.Method != "GET" {
		permission = "settlement.recognition.record"
		if strings.HasSuffix(r.URL.Path, "/decision") {
			permission = "settlement.recognition.review"
		}
	}
	p, ok := a.user(w, r, permission)
	if !ok {
		return true
	}
	path := strings.TrimPrefix(r.URL.Path, base)
	if a.serveRecognitionEvidence(w, r, p, path) {
		return true
	}
	if path == "/project-reference" && r.Method == "GET" {
		value, e := a.service.RecognitionProjectReference(r.Context(), p, r.URL.Query().Get("project_id"))
		if e != nil {
			recognitionError(w, e)
		} else {
			write(w, 200, value)
		}
		return true
	}
	if path == "/people" && r.Method == "GET" {
		a.recognitionPeople(w, r, p)
		return true
	}
	if path == "/candidates" && r.Method == "GET" {
		value, e := a.service.RecognitionCandidates(r.Context(), p, r.URL.Query().Get("claim_id"))
		if e != nil {
			recognitionError(w, e)
		} else {
			write(w, 200, value)
		}
		return true
	}
	if path == "" {
		a.recognitionCollection(w, r, p)
		return true
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 1 && parts[0] != "" && r.Method == "PATCH" {
		var in service.RecognitionEdit
		if !decode(w, r, &in) {
			return true
		}
		row, e := a.service.UpdateRecognition(r.Context(), p, parts[0], r.Header.Get("Idempotency-Key"), in)
		if e != nil {
			recognitionError(w, e)
		} else {
			write(w, 200, row)
		}
		return true
	}
	if len(parts) == 1 && parts[0] != "" && r.Method == "GET" {
		row, e := a.service.GetRecognition(r.Context(), p, parts[0], permission)
		if e != nil {
			recognitionError(w, e)
		} else {
			write(w, 200, row)
		}
		return true
	}
	if len(parts) == 2 && parts[0] != "" && r.Method == "POST" && (parts[1] == "submit" || parts[1] == "decision") {
		var in service.RecognitionDecision
		if !decode(w, r, &in) {
			return true
		}
		row, e := a.service.TransitionRecognition(r.Context(), p, parts[0], parts[1], r.Header.Get("Idempotency-Key"), in)
		if e != nil {
			recognitionError(w, e)
		} else {
			write(w, 200, row)
		}
		return true
	}
	fail(w, 404, "SETTLEMENT_NOT_FOUND", "未找到财务确认接口")
	return true
}

// recognitionCollection 处理有界范围列表与完整草稿登记。
// 参数：w/r为请求响应，p为可信当前主体。
// 返回值：无，写出真实记录或公开错误。
func (a *API) recognitionCollection(w http.ResponseWriter, r *http.Request, p service.Principal) {
	switch r.Method {
	case "GET":
		limit, offset, e := page(r)
		if e != nil {
			recognitionError(w, e)
			return
		}
		items, e := a.service.ListRecognitions(r.Context(), p, limit, offset)
		if e != nil {
			recognitionError(w, e)
			return
		}
		write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset, "reimbursement_configured": a.service.ReimbursementCosts != nil, "source_environment": a.service.RecognitionEnvironment, "basis": "本系统已登记财务确认；付款/预付不自动生成成本，历史未登记不代表零"})
	case "POST":
		var in service.RecognitionInput
		if !decode(w, r, &in) {
			return
		}
		row, e := a.service.CreateRecognition(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
		if e != nil {
			recognitionError(w, e)
			return
		}
		write(w, 201, row)
	default:
		fail(w, 405, "SETTLEMENT_METHOD_NOT_ALLOWED", "请求方法不支持")
	}
}

// recognitionPeople 返回受控目录候选并过滤当前财务动作的组织范围。
// 参数：w/r为请求，p为可信主体。
// 返回值：无，返回可选人员及其有权组织，不假造历史任职。
func (a *API) recognitionPeople(w http.ResponseWriter, r *http.Request, p service.Principal) {
	if a.service.RecognitionDirectory == nil {
		recognitionError(w, service.ErrDependency)
		return
	}
	people, e := a.service.RecognitionDirectory.RecognitionPeople(r.Context(), r.URL.Query().Get("user_id"), r.URL.Query().Get("keyword"))
	if e != nil {
		recognitionError(w, service.ErrDependency)
		return
	}
	result := []service.RecognitionPerson{}
	for _, person := range people {
		orgs := []service.RecognitionOrganization{}
		for _, org := range person.Organizations {
			if service.RecognitionOrganizationAllowed(p, org.ID) {
				orgs = append(orgs, org)
			}
		}
		if len(orgs) > 0 {
			person.Organizations = orgs
			result = append(result, person)
		}
	}
	write(w, 200, map[string]any{"items": result})
}

// dashboardMetrics 复用专属结算看板身份提供兼容指标包络。
// 参数：w/r为只读机器请求。
// 返回值：无，签名租户完整快照或明确来源不可用。
func (a *API) dashboardMetrics(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.DashboardMachineEnabled || a.dashboardMachine == nil {
		fail(w, 503, "SETTLEMENT_DASHBOARD_UNAVAILABLE", "结算看板机器用途尚未配置")
		return
	}
	bearer := r.Header.Get("Authorization")
	if !strings.HasPrefix(bearer, "Bearer ") {
		fail(w, 401, "SETTLEMENT_DASHBOARD_BEARER_REQUIRED", "必须携带专用机器令牌")
		return
	}
	identity, e := a.dashboardMachine.Verify(r.Context(), strings.TrimPrefix(bearer, "Bearer "))
	if e != nil || identity.TenantID == "" || identity.TenantID != a.cfg.OIDCTenantID {
		fail(w, 401, "SETTLEMENT_DASHBOARD_BEARER_INVALID", "机器令牌无效")
		return
	}
	snapshot, e := a.service.DashboardMetrics(r.Context(), identity.TenantID)
	if e != nil {
		fail(w, 503, "SETTLEMENT_DASHBOARD_SOURCE_UNAVAILABLE", "结算指标来源暂不可用，保留上次发布结果")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": "OK", "data": snapshot})
}

// recognitionError 保留受控财务冲正及费用限额原因，依赖故障不泄漏内部地址或SQL。
// 参数：w为响应，err为真实业务错误。
// 返回值：无；只对明确的领域错误输出可执行中文说明。
func recognitionError(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrConflict) {
		message := "确认记录已变化、重复或可确认余额不足，请刷新并核对来源"
		detail := strings.TrimPrefix(err.Error(), "conflict: ")
		if detail != err.Error() && len(detail) < 1000 && regexp.MustCompile(`\p{Han}`).MatchString(detail) {
			message = detail
		}
		fail(w, 409, "SETTLEMENT_RECOGNITION_CONFLICT", message)
		return
	}
	if errors.Is(err, service.ErrDependency) {
		fail(w, 503, "SETTLEMENT_RECOGNITION_DEPENDENCY_UNAVAILABLE", "受控人员、项目、报销来源或文件网关暂不可用，请检查专用接线后重试")
		return
	}
	respondError(w, err)
}
