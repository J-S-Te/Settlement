package httpapi

import (
	"net/http"
	"strings"

	"github.com/j-s-te/settlement/internal/service"
)

// listContractRevisionHolds 用途：财务查询合同修订人工复核工单。
// 参数：w为响应，r为已鉴权的列表请求；租户只取可信主体，查询串只承载筛选与分页。
// 返回值：无，当前页工单写入HTTP；责任范围不可见的工单不返回。
func (a *API) listContractRevisionHolds(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.revision.read")
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	items, err := a.service.ListContractRevisionHoldsPage(r.Context(), p, service.ContractRevisionHoldQuery{
		Limit:       limit,
		Offset:      offset,
		Status:      r.URL.Query().Get("status"),
		ContractRef: r.URL.Query().Get("contract_ref"),
	})
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
}

// resolveContractRevisionHold 用途：财务完成调整/红冲后人工关闭修订工单。
// 参数：w为响应，r为携带处理说明的关单请求，路径ID为工单标识。
// 返回值：无；reason 必填，同原因重放幂等成功且不重复写审计。
func (a *API) resolveContractRevisionHold(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.revision.resolve")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/contract-revision-holds/"), "/resolve")
	if id == "" || strings.Contains(id, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "修订工单标识不合法")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := a.service.ResolveContractRevisionHold(r.Context(), p, id, body.Reason); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"id": id, "status": "RESOLVED", "resolution_reason": strings.TrimSpace(body.Reason)})
}
