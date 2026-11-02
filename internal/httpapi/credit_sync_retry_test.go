package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/j-s-te/settlement/internal/service"
)

// TestCreditSyncRetryFailureMapsAuthorizationWithout503 验证重试端点的失败语义
// （BIZ-XAPI-02）：凭据/用途授权错误返回不可立即重试的 409，5xx/网络类失败仍为 503。
// 参数：t为测试上下文。
// 返回值：无；映射不一致时测试失败。
func TestCreditSyncRetryFailureMapsAuthorizationWithout503(t *testing.T) {
	authorization := fmt.Errorf("%w: CRM credit endpoint returned HTTP 401", service.ErrCreditSyncAuthorization)
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "authorization denied", err: authorization, wantStatus: http.StatusConflict, wantCode: "SETTLEMENT_CREDIT_SYNC_AUTHORIZATION_FAILED"},
		{name: "wrapped authorization denied", err: fmt.Errorf("retry: %w", authorization), wantStatus: http.StatusConflict, wantCode: "SETTLEMENT_CREDIT_SYNC_AUTHORIZATION_FAILED"},
		{name: "upstream failure", err: errors.New("CRM credit endpoint returned HTTP 503"), wantStatus: http.StatusServiceUnavailable, wantCode: "SETTLEMENT_CREDIT_SYNC_FAILED"},
		{name: "network failure", err: errors.New("dial tcp: connection refused"), wantStatus: http.StatusServiceUnavailable, wantCode: "SETTLEMENT_CREDIT_SYNC_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, message := creditSyncRetryFailure(tc.err)
			if status != tc.wantStatus || code != tc.wantCode || message == "" {
				t.Fatalf("mapping = (%d, %q, %q), want (%d, %q, non-empty)", status, code, message, tc.wantStatus, tc.wantCode)
			}
			if status == http.StatusServiceUnavailable && code == "SETTLEMENT_CREDIT_SYNC_AUTHORIZATION_FAILED" {
				t.Fatal("authorization failure must not be reported as retryable 503")
			}
		})
	}
}
