package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// DashboardIntegrationOptions 避免独立看板验证器与交接验证器发生接口装配混淆。
type DashboardIntegrationOptions struct{ Verifier handoverVerifier }

// dashboardFinance 仅向专用机器用途返回签名租户的财务快照。
// 参数：w为HTTP响应，r为包含平台机器Bearer的只读请求。
// 返回值：无，写出BusinessSnapshot原始JSON或明确错误。
func (a *API) dashboardFinance(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.DashboardMachineEnabled || a.dashboardMachine == nil {
		fail(w, 503, "SETTLEMENT_DASHBOARD_UNAVAILABLE", "结算看板机器用途尚未配置")
		return
	}
	const prefix = "Bearer "
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(authorization, prefix) || strings.TrimSpace(strings.TrimPrefix(authorization, prefix)) == "" {
		fail(w, 401, "SETTLEMENT_DASHBOARD_BEARER_REQUIRED", "结算看板调用必须携带专用机器令牌")
		return
	}
	identity, err := a.dashboardMachine.Verify(r.Context(), strings.TrimSpace(strings.TrimPrefix(authorization, prefix)))
	if err != nil || identity.TenantID == "" || identity.TenantID != a.cfg.OIDCTenantID {
		fail(w, 401, "SETTLEMENT_DASHBOARD_BEARER_INVALID", "结算看板机器令牌无效")
		return
	}
	value, err := a.service.DashboardFinance(r.Context(), identity.TenantID, time.Now())
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		if a.logger != nil {
			a.logger.Error("read finance dashboard", "error", err)
		}
		fail(w, status, "SETTLEMENT_DASHBOARD_SOURCE_UNAVAILABLE", "结算看板来源暂不可用")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(value)
}
