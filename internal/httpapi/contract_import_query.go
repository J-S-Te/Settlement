package httpapi

import (
	"database/sql"
	"encoding/json"
	"github.com/j-s-te/settlement/internal/service"
	"net/http"
	"strings"
)

// serveImportQueries 提供独立暂存工作台和源端恢复查询，关闭新增入口后仍可查询。
// 参数：w为响应，r为通过总入口许可和CSRF边界的请求。
// 返回值：匹配导入查询路由时返回真。
func (a *API) serveImportQueries(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	machine := strings.HasPrefix(r.URL.Path, "/internal/v1/settlement/contract-imports/")
	user := strings.HasPrefix(r.URL.Path, "/api/v1/settlement/contract-imports")
	if !machine && !user {
		return false
	}
	var tenant string
	prefix := "/api/v1/settlement/contract-imports"
	if machine {
		var ok bool
		tenant, ok = a.integration(r)
		if !ok {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_MACHINE_UNAUTHENTICATED", "服务间令牌无效")
			return true
		}
		if tenant == "" {
			fail(w, http.StatusForbidden, "SETTLEMENT_MACHINE_TENANT_UNRESOLVED", "服务间令牌未绑定可信租户")
			return true
		}
		prefix = "/internal/v1/settlement/contract-imports"
	} else {
		p, ok := a.user(w, r, "settlement.import.reconcile")
		if !ok {
			return true
		}
		if !p.Unrestricted("settlement.import.reconcile") {
			respondError(w, service.ErrForbidden)
			return true
		}
		tenant = p.TenantID
	}
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if !machine && strings.HasSuffix(id, "/facts") {
		a.importFacts(w, r, tenant, strings.TrimSuffix(id, "/facts"))
		return true
	}
	if machine && id == "capabilities" {
		write(w, http.StatusOK, map[string]any{"schema_version": 1, "prepare_enabled": a.cfg.ContractImportEnabled, "phases": []string{"PREPARE", "RELEASE"}, "import_modes": []string{"CONTINUE_LIVE", "HISTORICAL_CLOSED"}, "supported_fact_types": []string{"RECEIPT", "ALLOCATION"}, "blocked_fact_types": []string{"REVERSAL", "REFUND", "INVOICE", "RED_INVOICE"}, "expected_facts_required": true, "catalog_version": "4", "reconcile_permission": "settlement.import.reconcile", "release_permission": "settlement.import.release"})
		return true
	}
	if id != "" {
		result, err := a.importStatus(r, tenant, id)
		if err != nil {
			respondError(w, err)
			return true
		}
		write(w, http.StatusOK, result)
		return true
	}
	limit, offset, err := page(r)
	if err != nil {
		respondError(w, service.ErrInvalid)
		return true
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT contract_snapshot_id FROM settlement_contract_import WHERE tenant_id=? ORDER BY contract_snapshot_id LIMIT ? OFFSET ?`, tenant, limit, offset)
	if err != nil {
		respondError(w, err)
		return true
	}
	ids := []string{}
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			rows.Close()
			respondError(w, err)
			return true
		}
		ids = append(ids, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		respondError(w, err)
		return true
	}
	items := []map[string]any{}
	for _, value := range ids {
		item, e := a.importStatus(r, tenant, value)
		if e != nil {
			respondError(w, e)
			return true
		}
		items = append(items, item)
	}
	write(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
	return true
}

// importFacts 分页查询授权暂存合同的实际恢复映射，供未知响应后核对业务ID。
// 参数：w/r为已鉴权请求，tenant/id为可信租户和暂存快照。
// 返回值：无；返回最多一页来源映射，不暴露其他合同的凭证。
func (a *API) importFacts(w http.ResponseWriter, r *http.Request, tenant, id string) {
	if strings.Contains(id, "/") || id == "" {
		respondError(w, service.ErrNotFound)
		return
	}
	var version uint64
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT cs.import_snapshot_version FROM settlement_contract_snapshot cs JOIN settlement_contract_import ci ON ci.contract_snapshot_id=cs.id AND ci.tenant_id=cs.tenant_id WHERE cs.tenant_id=? AND cs.id=?`, tenant, id).Scan(&version)
	if err == sql.ErrNoRows {
		respondError(w, service.ErrNotFound)
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		respondError(w, service.ErrInvalid)
		return
	}
	var total int
	if err = a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM settlement_import_fact WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, id).Scan(&total); err != nil {
		respondError(w, err)
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT source_system,fact_type,external_fact_id,target_business_id FROM settlement_import_fact WHERE tenant_id=? AND contract_snapshot_id=? ORDER BY source_system,fact_type,external_fact_id LIMIT ? OFFSET ?`, tenant, id, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]string{}
	for rows.Next() {
		var source, kind, external, target string
		if err = rows.Scan(&source, &kind, &external, &target); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]string{"source_system": source, "fact_type": kind, "external_fact_id": external, "target_business_id": target})
	}
	if err = rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"contract_snapshot_id": id, "snapshot_version": version, "items": items, "total": total, "limit": limit, "offset": offset})
}

// importStatus 查询本地财务和责任投影的实际版本，源端据此判断独立放行前置。
// 参数：r为查询上下文，tenant为可信租户，id为已暂存财务快照。
// 返回值：带HOLD标识、计划及权威责任ACK的状态；不可见返回不存在。
func (a *API) importStatus(r *http.Request, tenant, id string) (map[string]any, error) {
	var state, migration, item, reconciliation, currency, amount, owner, actualOwner, decision string
	var version, release, source, ownerVersion uint64
	var active bool
	var manifest []byte
	var customerID, customerName, contractID, contractNo, policyDigest, importMode string
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT ci.state,cs.migration_state,ci.origin_import_item_id,ci.reconciliation_id,cs.currency,cs.contract_amount,ci.owner_user_id,cs.import_snapshot_version,ci.release_version,ci.source_revision,COALESCE(cr.owner_user_id,''),COALESCE(cr.version,0),COALESCE(cr.active,FALSE),ci.credit_decision,ci.expected_facts_json,cs.customer_id,cs.customer_name_snapshot,cs.source_contract_id,cs.source_contract_no,ci.policy_digest,ci.import_mode FROM settlement_contract_import ci JOIN settlement_contract_snapshot cs ON cs.id=ci.contract_snapshot_id AND cs.tenant_id=ci.tenant_id LEFT JOIN settlement_contract_responsibility cr ON cr.source_contract_id=cs.source_contract_id AND cr.tenant_id=cs.tenant_id WHERE ci.tenant_id=? AND ci.contract_snapshot_id=?`, tenant, id).Scan(&state, &migration, &item, &reconciliation, &currency, &amount, &owner, &version, &release, &source, &actualOwner, &ownerVersion, &active, &decision, &manifest, &customerID, &customerName, &contractID, &contractNo, &policyDigest, &importMode)
	if err == sql.ErrNoRows {
		return nil, service.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT rp.id,rp.installment_no,DATE_FORMAT(rp.due_date,'%Y-%m-%d'),rp.planned_amount,rp.confirmation_status,rp.version,COALESCE(rv.id,''),COALESCE(rv.open_amount,0) FROM settlement_receivable_plan rp LEFT JOIN settlement_receivable rv ON rv.receivable_plan_id=rp.id AND rv.tenant_id=rp.tenant_id WHERE rp.tenant_id=? AND rp.contract_snapshot_id=? ORDER BY rp.installment_no,rp.id`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := []map[string]any{}
	for rows.Next() {
		var plan, due, planned, status, receivable, open string
		var installment, planVersion int
		if err = rows.Scan(&plan, &installment, &due, &planned, &status, &planVersion, &receivable, &open); err != nil {
			return nil, err
		}
		plans = append(plans, map[string]any{"id": plan, "installment_no": installment, "due_date": due, "planned_amount": planned, "confirmation_status": status, "version": planVersion, "receivable_id": receivable, "open_amount": open})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"policy_digest": policyDigest, "import_mode": importMode, "customer_id": customerID, "customer_name": customerName, "source_contract_id": contractID, "source_contract_no": contractNo, "contract_snapshot_id": id, "state": state, "migration_state": migration, "origin_import_item_id": item, "snapshot_version": version, "release_version": release, "source_revision": source, "reconciliation_id": reconciliation, "currency": currency, "contract_amount": amount, "expected_owner_user_id": owner, "owner_user_id": actualOwner, "owner_version": ownerVersion, "owner_active": active, "owner_matches": active && actualOwner == owner, "credit_decision": decision, "expected_facts": json.RawMessage(manifest), "plans": plans}, nil
}
