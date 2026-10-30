package httpapi

import (
	"errors"
	"github.com/j-s-te/settlement/internal/service"
	"net/http"
	"strings"
)

// serveContractImports 接入导入阶段端点并复用原机器、会话、CSRF及商业许可门禁。
// 参数：w为响应，r为当前请求。
// 返回值：属于导入路由时返回真，响应承载真实受理结果。
func (a *API) serveContractImports(w http.ResponseWriter, r *http.Request) bool {
	if a.serveImportExports(w, r) {
		return true
	}
	if a.serveImportQueries(w, r) {
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/settlement/contract-imports/") && strings.HasSuffix(r.URL.Path, "/reconcile") && r.Method == http.MethodPost {
		p, ok := a.user(w, r, "settlement.import.reconcile")
		if !ok {
			return true
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/settlement/contract-imports/"), "/reconcile")
		var in service.ImportReconcileInput
		if !decode(w, r, &in) {
			return true
		}
		result, err := a.service.ReconcileContractImport(r.Context(), p, id, in)
		if err != nil {
			respondImportError(w, err)
			return true
		}
		write(w, http.StatusOK, result)
		return true
	}
	prepare := r.URL.Path == "/internal/v1/settlement/events/contract-imports"
	release := strings.HasPrefix(r.URL.Path, "/internal/v1/settlement/contract-imports/") && strings.HasSuffix(r.URL.Path, "/release")
	if !prepare && !release {
		return false
	}
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "SETTLEMENT_METHOD_NOT_ALLOWED", "请求方法不支持")
		return true
	}
	tenant, ok := a.integration(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "SETTLEMENT_MACHINE_UNAUTHENTICATED", "服务间令牌无效")
		return true
	}
	if strings.TrimSpace(tenant) == "" {
		fail(w, http.StatusForbidden, "SETTLEMENT_MACHINE_TENANT_UNRESOLVED", "服务间令牌未绑定可信租户")
		return true
	}
	var in service.ContractImportCommand
	if !decode(w, r, &in) {
		return true
	}
	if release {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/internal/v1/settlement/contract-imports/"), "/release")
		receipt, err := a.service.ReleaseContractImport(r.Context(), tenant, id, in)
		if err != nil {
			respondImportError(w, err)
			return true
		}
		write(w, http.StatusAccepted, receipt)
		return true
	}
	receipt, err := a.service.PrepareContractImport(r.Context(), tenant, in)
	if err != nil {
		respondImportError(w, err)
		return true
	}
	write(w, http.StatusAccepted, receipt)
	return true
}

// respondImportError 对不支持的历史事实返回显式阻塞，复用其余结算错误契约。
// 参数：w为响应，err为真实业务错误。
// 返回值：无，HTTP错误响应说明阻塞原因。
func respondImportError(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrImportBlocked) {
		fail(w, http.StatusConflict, "SETTLEMENT_IMPORT_FACT_BLOCKED", "历史财务事实未恢复或尚不支持安全恢复，禁止对账及放行")
		return
	}
	respondError(w, err)
}
