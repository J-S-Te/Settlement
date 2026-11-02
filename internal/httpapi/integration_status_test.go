package httpapi

import (
	"database/sql/driver"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

func integrationTestHandler(t *testing.T, cfg config.Config, steps ...scriptdb.Step) http.Handler {
	t.Helper()
	db := scriptdb.Open(t, steps...)
	return New(db, cfg, slog.New(slog.NewTextHandler(&discardWriter{}, nil)), nil, nil, nil)
}

type discardWriter struct{}

func (*discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func pendingStep(rows ...[]driver.Value) scriptdb.Step {
	return scriptdb.Step{Kind: "query", SQL: "GROUP BY destination", Rows: rows}
}

func serveIntegration(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	return response
}

// TestReadyzFailsClosedWhenRequiredAuditDestinationIsMissing 覆盖 required 审计缺失 ⇒ 503。
func TestReadyzFailsClosedWhenRequiredAuditDestinationIsMissing(t *testing.T) {
	handler := integrationTestHandler(t, config.Config{
		AuditDestinationRequired:          true,
		AuditDestinationConfigured:        false,
		NotificationDestinationConfigured: true,
	}, pendingStep([]driver.Value{"PLATFORM_AUDIT", int64(2)}))

	response := serveIntegration(t, handler, "/readyz")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "SETTLEMENT_AUDIT_DESTINATION_REQUIRED") {
		t.Fatalf("body = %s, want required-audit rejection code", response.Body.String())
	}
}

// TestReadyzDegradesForOptionalAuditAndUnconfiguredBacklog 覆盖非 required 缺失与
// 未配置目的地积压 ⇒ 200 degraded（不阻断启动但可见）。
func TestReadyzDegradesForOptionalAuditAndUnconfiguredBacklog(t *testing.T) {
	handler := integrationTestHandler(t, config.Config{
		AuditDestinationRequired:          false,
		AuditDestinationConfigured:        false,
		NotificationDestinationConfigured: false,
	}, pendingStep(
		[]driver.Value{"PLATFORM_AUDIT", int64(2)},
		[]driver.Value{"TAX_INVOICE_COMMAND", int64(3)},
	))

	response := serveIntegration(t, handler, "/readyz")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 degraded; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Status                    string                    `json:"status"`
			Integrations              map[string]map[string]any `json:"integrations"`
			OutboxPending             map[string]int64          `json:"outbox_pending"`
			UnconfiguredDestinations  []string                  `json:"unconfigured_destinations"`
			UnconfiguredOutboxPending int64                     `json:"unconfigured_outbox_pending"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", payload.Data.Status)
	}
	if payload.Data.Integrations["audit"]["required"] != false || payload.Data.Integrations["audit"]["configured"] != false {
		t.Fatalf("audit integration = %v", payload.Data.Integrations["audit"])
	}
	if payload.Data.UnconfiguredOutboxPending != 5 || len(payload.Data.UnconfiguredDestinations) != 2 {
		t.Fatalf("unconfigured backlog = %d %v", payload.Data.UnconfiguredOutboxPending, payload.Data.UnconfiguredDestinations)
	}
	if payload.Data.OutboxPending["PLATFORM_AUDIT"] != 2 {
		t.Fatalf("outbox_pending = %v", payload.Data.OutboxPending)
	}
}

// TestReadyzIsReadyWhenEveryDestinationIsConfigured 覆盖目的地齐全时的 ready 语义。
func TestReadyzIsReadyWhenEveryDestinationIsConfigured(t *testing.T) {
	handler := integrationTestHandler(t, config.Config{
		AuditDestinationRequired:          true,
		AuditDestinationConfigured:        true,
		NotificationDestinationConfigured: true,
		InvoiceIssuanceMode:               "tax_adapter",
	}, pendingStep())

	response := serveIntegration(t, handler, "/readyz")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"status":"ready"`) {
		t.Fatalf("body = %s, want ready", response.Body.String())
	}
}

// TestMetricsExposePendingAndUnconfiguredDestinationCounts 覆盖 /metrics 的
// *_outbox_pending{destination} 与未配置目的地计数。
func TestMetricsExposePendingAndUnconfiguredDestinationCounts(t *testing.T) {
	handler := integrationTestHandler(t, config.Config{
		AuditDestinationRequired:          false,
		AuditDestinationConfigured:        false,
		NotificationDestinationConfigured: false,
	}, pendingStep(
		[]driver.Value{"PLATFORM_AUDIT", int64(2)},
		[]driver.Value{"TAX_INVOICE_COMMAND", int64(3)},
	))

	response := serveIntegration(t, handler, "/metrics")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`settlement_outbox_pending{destination="PLATFORM_AUDIT"} 2`,
		`settlement_outbox_pending{destination="TAX_INVOICE_COMMAND"} 3`,
		`settlement_integration_destination_configured{destination="PLATFORM_AUDIT"} 0`,
		`settlement_outbox_unconfigured_pending 5`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
}

// TestEvaluateReadinessRendersDegradedDestinations 直接覆盖纯判定，
// 保证 required 缺失时即使其它目的地齐全也返回 503。
func TestEvaluateReadinessRendersDegradedDestinations(t *testing.T) {
	status, body := evaluateReadiness(integrationSnapshot{
		AuditRequired:          true,
		AuditConfigured:        false,
		NotificationConfigured: true,
		Pending:                map[string]int64{"PLATFORM_AUDIT": 4},
	})
	if status != http.StatusServiceUnavailable || body["status"] != "not_ready" {
		t.Fatalf("evaluateReadiness() = %d %v, want 503 not_ready", status, body)
	}
	if body["unconfigured_outbox_pending"] != int64(4) {
		t.Fatalf("unconfigured_outbox_pending = %v, want 4", body["unconfigured_outbox_pending"])
	}
}
