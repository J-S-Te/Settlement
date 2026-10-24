package httpapi

import (
	"net/http"
	"strings"
)

func (a *API) recoverInvoiceFailure(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.approve")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-requests/"), "/recover-failure")
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
	if err := a.service.RecoverFailedInvoice(r.Context(), p, r.Header.Get("Idempotency-Key"), id, body.Version, body.Reason); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"id": id, "status": "SUBMITTED", "version": body.Version + 1})
}
