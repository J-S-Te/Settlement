package httpapi

import (
	"github.com/j-s-te/settlement/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestImportCommandMachineBoundary 验证恢复接口先校验机器身份且关闭入口不误受理。
// 参数：t为测试上下文。
// 返回值：无，缺少机器鉴权或关闭开关仍受理时失败。
func TestImportCommandMachineBoundary(t *testing.T) {
	handler := newMachineIngestHandler(config.Config{IntegrationEnabled: true})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contract-imports", strings.NewReader(`{}`)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("want machine authentication rejection, got %d %s", response.Code, response.Body.String())
	}
}

// TestImportProtocolRejectsBeforeDatabase 验证越租户、未知协议和不支持事实均不能触达数据库。
// 参数：t为测试上下文。
// 返回值：无，缺少严格协议或可信租户门禁即失败。
func TestImportProtocolRejectsBeforeDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		enabled    bool
		status     int
		code       string
	}{
		{"cross-tenant", `{"tenant_id":"other"}`, true, 400, "SETTLEMENT_INVALID_REQUEST"},
		{"unknown-schema", `{"event_id":"E1","event_type":"contract.import_financial.v1","schema_version":2,"phase":"PREPARE","tenant_id":"tenant","origin_import_item_id":"item","source_revision":1,"release_version":1,"policy_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","import_mode":"CONTINUE_LIVE","occurred_at":"2020-01-01T00:00:00Z","expected_facts":[]}`, true, 400, "SETTLEMENT_INVALID_REQUEST"},
		{"unsupported-invoice", `{"event_id":"E1","event_type":"contract.import_financial.v1","schema_version":1,"phase":"PREPARE","tenant_id":"tenant","origin_import_item_id":"item","source_revision":1,"release_version":1,"policy_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","import_mode":"CONTINUE_LIVE","occurred_at":"2020-01-01T00:00:00Z","expected_facts":[{"source_system":"legacy","fact_type":"INVOICE","external_fact_id":"old-invoice"}]}`, true, 409, "SETTLEMENT_IMPORT_FACT_BLOCKED"},
		{"default-disabled", `{"event_id":"E1","event_type":"contract.import_financial.v1","schema_version":1,"phase":"PREPARE","tenant_id":"tenant","origin_import_item_id":"item","source_revision":1,"release_version":1,"policy_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","import_mode":"CONTINUE_LIVE","occurred_at":"2020-01-01T00:00:00Z","expected_facts":[]}`, false, 403, "SETTLEMENT_FORBIDDEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := newMachineIngestHandler(config.Config{IntegrationEnabled: true, DevelopmentAuth: true, IntegrationBearerToken: "explicit-test-token", OIDCTenantID: "tenant", ContractImportEnabled: tc.enabled})
			r := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contract-imports", strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer explicit-test-token")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
