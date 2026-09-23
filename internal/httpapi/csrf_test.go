package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
)

// newCSRFTestHandler 构造无依赖的 API：CSRF 校验发生在路由分派之前，
// 被拒绝的请求不会触达数据库。
func newCSRFTestHandler(publicOrigin string) http.Handler {
	return New(nil, config.Config{PublicOrigin: publicOrigin}, slog.Default(), nil, nil, nil)
}

// SEC-D11：/api/v1 写请求的 Origin 必须存在且与 PublicOrigin 一致；
// 缺失、跨源、缺 CSRF 头、Sec-Fetch-Site=cross-site 一律被拒。
func TestSettlementWriteRejectsMissingAndCrossOrigin(t *testing.T) {
	tests := []struct {
		name         string
		publicOrigin string
		origin       string
		csrf         string
		secFetchSite string
	}{
		{name: "missing origin", publicOrigin: "https://platform.example.com", csrf: "1"},
		{name: "cross origin", publicOrigin: "https://platform.example.com", origin: "https://evil.example", csrf: "1"},
		{name: "missing csrf header", publicOrigin: "https://platform.example.com", origin: "https://platform.example.com"},
		{name: "empty public origin without origin header", publicOrigin: "", csrf: "1"},
		{name: "empty public origin with any origin header", publicOrigin: "", origin: "https://platform.example.com", csrf: "1"},
		{name: "cross-site fetch metadata despite matching origin", publicOrigin: "https://platform.example.com", origin: "https://platform.example.com", csrf: "1", secFetchSite: "cross-site"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newCSRFTestHandler(test.publicOrigin)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/receipts", strings.NewReader("{}"))
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.csrf != "" {
				request.Header.Set("X-CSRF-Token", test.csrf)
			}
			if test.secFetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.secFetchSite)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "SETTLEMENT_CSRF_REJECTED") {
				t.Fatalf("status=%d body=%s, want 403 SETTLEMENT_CSRF_REJECTED", response.Code, response.Body.String())
			}
		})
	}
}

// SEC-D11 正向基线：同源 + CSRF 头可通过来源校验（走到业务分派，此处为 404 兜底），
// 且 GET 不受来源校验影响。
func TestSettlementWriteAllowsMatchingOriginAndLeavesReadsUntouched(t *testing.T) {
	handler := newCSRFTestHandler("https://platform.example.com")

	write := httptest.NewRequest(http.MethodPost, "/api/v1/definitely-unknown", strings.NewReader("{}"))
	write.Header.Set("Origin", "https://platform.example.com")
	write.Header.Set("X-CSRF-Token", "1")
	writeResponse := httptest.NewRecorder()
	handler.ServeHTTP(writeResponse, write)
	if writeResponse.Code != http.StatusNotFound {
		t.Fatalf("write status=%d body=%s, want 404 (CSRF passed, default route)", writeResponse.Code, writeResponse.Body.String())
	}

	read := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	readResponse := httptest.NewRecorder()
	handler.ServeHTTP(readResponse, read)
	if readResponse.Code != http.StatusOK {
		t.Fatalf("read status=%d, want 200 (reads are exempt)", readResponse.Code)
	}
}
