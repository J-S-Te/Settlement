package httpapi

import (
	"net/http"
	"strings"
)

// cancelInvoiceRequest 用途：作废确定未被税控受理的开票申请并释放额度预留。
// 参数：w为响应，r为携带幂等键、当前版本与作废原因的请求。
// 返回值：无；成功仅把申请置为已作废、尝试置为放弃并释放未开票预留。
func (a *API) cancelInvoiceRequest(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.issue")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-requests/"), "/cancel")
	if id == "" || strings.Contains(id, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "开票申请编号无效")
		return
	}
	var body struct {
		Version int    `json:"version"`
		Reason  string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := a.service.CancelInvoiceRequest(r.Context(), p, r.Header.Get("Idempotency-Key"), id, body.Version, body.Reason); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"id": id, "status": "CANCELLED", "version": body.Version + 1})
}
