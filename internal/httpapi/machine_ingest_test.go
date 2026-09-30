package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
)

// newMachineIngestHandler 构造无数据库的 API：机器边界的租户校验全部发生在
// 落库之前，被拒绝的请求不会触达数据库（IngestContract/ApplyTaxCallback 的
// 租户一致性检查先于任何 BeginTx）。
func newMachineIngestHandler(cfg config.Config) http.Handler {
	return New(nil, cfg, slog.Default(), nil, nil, nil)
}

// AUD-2026-001：未携带服务间令牌的合同事件摄取必须 401。
func TestContractEventRejectsUnauthenticated(t *testing.T) {
	handler := newMachineIngestHandler(config.Config{
		DevelopmentAuth: true, IntegrationEnabled: true, IntegrationBearerToken: "machine-secret",
		OIDCTenantID: "tenant-a", PublicOrigin: "http://localhost:5173",
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "SETTLEMENT_MACHINE_UNAUTHENTICATED") {
		t.Fatalf("status=%d body=%s, want 401 SETTLEMENT_MACHINE_UNAUTHENTICATED", response.Code, response.Body.String())
	}
}

// AUD-2026-001：可信租户缺失（验签身份/配置都拿不到）时必须失败关闭，
// 绝不采信报文体声明的租户。
func TestContractEventRejectsUnresolvedTrustedTenant(t *testing.T) {
	handler := newMachineIngestHandler(config.Config{
		DevelopmentAuth: true, IntegrationEnabled: true, IntegrationBearerToken: "machine-secret",
		OIDCTenantID: "", PublicOrigin: "http://localhost:5173",
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", strings.NewReader(`{"event_id":"E-1","tenant_id":"dev"}`))
	request.Header.Set("Authorization", "Bearer machine-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "SETTLEMENT_MACHINE_TENANT_UNRESOLVED") {
		t.Fatalf("status=%d body=%s, want 403 SETTLEMENT_MACHINE_TENANT_UNRESOLVED", response.Code, response.Body.String())
	}
}

// AUD-2026-001：报文体租户与可信租户不一致必须被拒绝（服务层 ErrInvalid 经
// respondError 映射为既有错误码 400），持合法令牌也无法跨租户写入。
func TestContractEventRejectsCrossTenant(t *testing.T) {
	handler := newMachineIngestHandler(config.Config{
		DevelopmentAuth: true, IntegrationEnabled: true, IntegrationBearerToken: "machine-secret",
		OIDCTenantID: "tenant-a", PublicOrigin: "http://localhost:5173",
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", strings.NewReader(`{"event_id":"E-1","tenant_id":"tenant-b"}`))
	request.Header.Set("Authorization", "Bearer machine-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "SETTLEMENT_INVALID_REQUEST") {
		t.Fatalf("status=%d body=%s, want 400 SETTLEMENT_INVALID_REQUEST", response.Code, response.Body.String())
	}
}

// AUD-2026-001：报文体缺失租户同样落入一致性校验被拒。
func TestContractEventRejectsMissingBodyTenant(t *testing.T) {
	handler := newMachineIngestHandler(config.Config{
		DevelopmentAuth: true, IntegrationEnabled: true, IntegrationBearerToken: "machine-secret",
		OIDCTenantID: "tenant-a", PublicOrigin: "http://localhost:5173",
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", strings.NewReader(`{"event_id":"E-1"}`))
	request.Header.Set("Authorization", "Bearer machine-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "SETTLEMENT_INVALID_REQUEST") {
		t.Fatalf("status=%d body=%s, want 400 SETTLEMENT_INVALID_REQUEST", response.Code, response.Body.String())
	}
}

// AUD-2026-020：DevelopmentAuth 且 OIDC_TENANT_ID 为空时，税控回调必须拒绝，
// 而不是回退信任报文体声明的租户。
func TestTaxResultRejectsUnresolvedTrustedTenant(t *testing.T) {
	handler := newMachineIngestHandler(config.Config{
		DevelopmentAuth: true, TaxResultIngestEnabled: true, TaxResultBearerToken: "tax-secret",
		OIDCTenantID: "", PublicOrigin: "http://localhost:5173",
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/tax-results", strings.NewReader(`{"event_id":"T-1","event_type":"tax.invoice.issued.v1","tenant_id":"dev"}`))
	request.Header.Set("Authorization", "Bearer tax-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "SETTLEMENT_TAX_TENANT_UNRESOLVED") {
		t.Fatalf("status=%d body=%s, want 403 SETTLEMENT_TAX_TENANT_UNRESOLVED", response.Code, response.Body.String())
	}
}

// AUD-2026-020 正向基线：OIDC_TENANT_ID 已配置时请求通过租户门禁进入服务层
// （此处因缺少 provider_code 被服务层校验拒绝为 400，而非租户门禁的 403）。
func TestTaxResultWithConfiguredTenantReachesService(t *testing.T) {
	handler := newMachineIngestHandler(config.Config{
		DevelopmentAuth: true, TaxResultIngestEnabled: true, TaxResultBearerToken: "tax-secret",
		OIDCTenantID: "dev", PublicOrigin: "http://localhost:5173",
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/tax-results", strings.NewReader(`{"event_id":"T-1","event_type":"tax.invoice.issued.v1","tenant_id":"dev"}`))
	request.Header.Set("Authorization", "Bearer tax-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "SETTLEMENT_INVALID_REQUEST") {
		t.Fatalf("status=%d body=%s, want 400 SETTLEMENT_INVALID_REQUEST from service validation", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "SETTLEMENT_TAX_TENANT_UNRESOLVED") {
		t.Fatalf("configured tenant must not be treated as unresolved: %s", response.Body.String())
	}
}
