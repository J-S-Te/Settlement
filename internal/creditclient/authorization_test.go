package creditclient

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"golang.org/x/oauth2"
)

// TestAuthorizationDeniedClassifiesPurposeAndCredentialFailures 验证授权/用途类
// 失败可与 5xx 区分：CRM 入口 401/403、平台令牌 invalid_client/invalid_scope
// 都是配置漂移（BIZ-XAPI-02），重试不会自愈；5xx 与网络错误仍按可重试处理。
// 参数：t为测试上下文。
// 返回值：无；分类结果不一致时测试失败。
func TestAuthorizationDeniedClassifiesPurposeAndCredentialFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "crm 401", err: &DeliveryError{StatusCode: http.StatusUnauthorized}, want: true},
		{name: "crm 403", err: &DeliveryError{StatusCode: http.StatusForbidden}, want: true},
		{name: "wrapped crm 401", err: fmt.Errorf("publish: %w", &DeliveryError{StatusCode: http.StatusUnauthorized}), want: true},
		{name: "crm 500", err: &DeliveryError{StatusCode: http.StatusInternalServerError}},
		{name: "crm 503", err: &DeliveryError{StatusCode: http.StatusServiceUnavailable}},
		{name: "network", err: errors.New("dial tcp: connection refused")},
		{name: "invalid_scope", err: &oauth2.RetrieveError{ErrorCode: "invalid_scope", Response: &http.Response{StatusCode: http.StatusBadRequest}}, want: true},
		{name: "invalid_client", err: &oauth2.RetrieveError{ErrorCode: "invalid_client", Response: &http.Response{StatusCode: http.StatusUnauthorized}}, want: true},
		{name: "token 401 without code", err: &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusUnauthorized}}, want: true},
		{name: "token 429", err: &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusTooManyRequests}}},
		{name: "license paused", err: ErrLicensePaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthorizationDenied(tc.err); got != tc.want {
				t.Fatalf("AuthorizationDenied(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
