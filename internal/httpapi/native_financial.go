package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/j-s-te/settlement/internal/service"
)

// serveNativeFinancial 注册在既有许可与CSRF外壳之后的本地财务接口。
// 参数：w/r 为经过公共安全边界的响应和请求。
// 返回值：属于本组路由时为真；所有写入仍执行服务端权限及范围校验。
func (a *API) serveNativeFinancial(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if path != "/api/v1/financial-customers" && !strings.HasPrefix(path, "/api/v1/financial-baselines") {
		return false
	}
	if path == "/api/v1/financial-baselines/capabilities" && r.Method == http.MethodGet {
		p, ok := a.user(w, r, "settlement.receivable.read")
		if !ok {
			return true
		}
		write(w, http.StatusOK, map[string]any{"native_financial_enabled": a.service.NativeFinancialEnabled, "can_create": a.service.CanCreateNativeFinancial(p), "crm_credit_subscribed": a.cfg.CRMCreditSyncEnabled})
		return true
	}
	permission := "settlement.receivable.read"
	if r.Method != http.MethodGet {
		permission = "settlement.receivable.create"
	}
	p, ok := a.user(w, r, permission)
	if !ok {
		return true
	}
	if path == "/api/v1/financial-customers" {
		a.nativeFinancialCustomers(w, r, p)
		return true
	}
	if path == "/api/v1/financial-baselines" {
		a.nativeFinancialBaselines(w, r, p)
		return true
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/financial-baselines/"), "/")
	if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodGet {
		value, err := a.service.GetNativeFinancialBaseline(r.Context(), p, parts[0])
		if err != nil {
			nativeFinancialError(w, err)
			return true
		}
		write(w, http.StatusOK, value)
		return true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "void" && r.Method == http.MethodPost {
		var input service.NativeFinancialVoidInput
		if !decode(w, r, &input) {
			return true
		}
		err := a.service.VoidNativeFinancialBaseline(r.Context(), p, parts[0], r.Header.Get("Idempotency-Key"), input)
		if err != nil {
			nativeFinancialError(w, err)
			return true
		}
		write(w, http.StatusOK, map[string]string{"id": parts[0], "status": "VOID"})
		return true
	}
	fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到接口")
	return true
}

// nativeFinancialCustomers 处理已有权限范围内的独立客户登记和分页读取。
// 参数：w/r 为HTTP请求，p 为已认证主体。
// 返回值：无；响应为真实登记结果或错误。
func (a *API) nativeFinancialCustomers(w http.ResponseWriter, r *http.Request, p service.Principal) {
	switch r.Method {
	case http.MethodGet:
		limit, offset, err := page(r)
		if err != nil {
			nativeFinancialError(w, service.ErrInvalid)
			return
		}
		items, err := a.service.ListNativeFinancialCustomers(r.Context(), p, limit, offset)
		if err != nil {
			nativeFinancialError(w, err)
			return
		}
		write(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
	case http.MethodPost:
		var input service.NativeCustomerInput
		if !decode(w, r, &input) {
			return
		}
		value, err := a.service.CreateNativeFinancialCustomer(r.Context(), p, r.Header.Get("Idempotency-Key"), input)
		if err != nil {
			nativeFinancialError(w, err)
			return
		}
		write(w, http.StatusCreated, value)
	default:
		fail(w, http.StatusMethodNotAllowed, "SETTLEMENT_METHOD_NOT_ALLOWED", "请求方法不支持")
	}
}

// nativeFinancialBaselines 处理本地依据的受权创建和有界历史查询。
// 参数：w/r 为HTTP请求，p 为已认证主体。
// 返回值：无；不会通过浏览器入口制造外部合同事件。
func (a *API) nativeFinancialBaselines(w http.ResponseWriter, r *http.Request, p service.Principal) {
	switch r.Method {
	case http.MethodGet:
		limit, offset, err := page(r)
		if err != nil {
			nativeFinancialError(w, service.ErrInvalid)
			return
		}
		items, err := a.service.ListNativeFinancialBaselines(r.Context(), p, limit, offset)
		if err != nil {
			nativeFinancialError(w, err)
			return
		}
		write(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
	case http.MethodPost:
		var input service.NativeFinancialInput
		if !decode(w, r, &input) {
			return
		}
		value, err := a.service.CreateNativeFinancialBaseline(r.Context(), p, r.Header.Get("Idempotency-Key"), input)
		if err != nil {
			nativeFinancialError(w, err)
			return
		}
		write(w, http.StatusCreated, value)
	default:
		fail(w, http.StatusMethodNotAllowed, "SETTLEMENT_METHOD_NOT_ALLOWED", "请求方法不支持")
	}
}

// nativeFinancialError 对本地能力关闭与已消费基线返回明确错误，保留原全局错误契约。
// 参数：w 为响应，err 为服务错误。
// 返回值：无；不将关闭能力或作废拒绝写成成功。
func nativeFinancialError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrNativeFinancialDisabled):
		fail(w, http.StatusConflict, "SETTLEMENT_NATIVE_FINANCIAL_DISABLED", "本地财务依据能力未启用")
	case errors.Is(err, service.ErrNativeFinancialConsumed):
		fail(w, http.StatusConflict, "SETTLEMENT_NATIVE_FINANCIAL_CONSUMED", "已有确认计划或正式账务，不能作废财务依据，请使用原调整、冲销或红票流程")
	default:
		respondError(w, err)
	}
}
