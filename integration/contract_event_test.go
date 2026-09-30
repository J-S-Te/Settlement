package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/httpapi"
)

// TestContractEventIngestTenantBoundary proves the machine-boundary tenant
// consistency guard for contract event ingestion (AUD-2026-001): a same-tenant
// event is accepted (202), a cross-tenant declaration is rejected, and an
// unauthenticated request never passes the machine boundary.
//
// Like TestInvoiceFlow this test is opt-in because it writes to the database:
//
//	SETTLEMENT_E2E_TEST=1 SETTLEMENT_E2E_TEST_DSN='...' go test ./integration -v
func TestContractEventIngestTenantBoundary(t *testing.T) {
	if os.Getenv("SETTLEMENT_E2E_TEST") != "1" {
		t.Skip("set SETTLEMENT_E2E_TEST=1 to run the database-backed business flow")
	}
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_E2E_TEST_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("SETTLEMENT_MYSQL_DSN"))
	}
	if dsn == "" {
		t.Fatal("SETTLEMENT_E2E_TEST_DSN or SETTLEMENT_MYSQL_DSN is required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("database is not reachable: %v", err)
	}

	const tenant = "dev"
	const source = "integration-test"
	suffix := strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	acceptedEventID := "E2E-CE-" + suffix
	rejectedEventID := "E2E-CE-R-" + suffix
	contractID := "E2E-CE-CONTRACT-" + suffix
	defer cleanupContractEvent(t, db, tenant, source, acceptedEventID, contractID)
	defer cleanupContractEvent(t, db, "other-tenant", source, rejectedEventID, contractID)

	handler := httpapi.New(db, config.Config{
		DevelopmentAuth: true, IntegrationEnabled: true, IntegrationBearerToken: "e2e-machine-secret",
		OIDCTenantID: tenant, PublicOrigin: "http://localhost:5173",
	}, slog.Default(), nil, nil, nil)

	event := func(eventID, declaredTenant string) map[string]any {
		return map[string]any{
			"event_id": eventID, "event_type": "contract.financial_effective.v1", "tenant_id": declaredTenant,
			"occurred_at": time.Now().UTC().Format(time.RFC3339),
			"aggregate":   map[string]any{"id": contractID, "version": 1},
			"contract": map[string]any{
				"id": contractID, "no": "E2E-CE-HT-" + suffix, "version": 1,
				"customer_id": "customer-e2e", "customer_name": "集成测试客户",
				"project_id": "project-e2e", "project_name": "集成测试项目",
				"currency": "CNY", "amount": "1000.00",
				"payment_terms": []map[string]any{{"installment_no": 1, "due_date": "2099-12-31", "amount": "1000.00"}},
			},
		}
	}

	unauthenticated := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", jsonBody(t, event(acceptedEventID, tenant)))
	unauthenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("contract event without machine token status=%d, want 401", unauthenticatedResponse.Code)
	}

	crossTenant := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", jsonBody(t, event(rejectedEventID, "other-tenant")))
	crossTenant.Header.Set("Authorization", "Bearer e2e-machine-secret")
	crossTenantResponse := httptest.NewRecorder()
	handler.ServeHTTP(crossTenantResponse, crossTenant)
	if crossTenantResponse.Code != http.StatusBadRequest || !strings.Contains(crossTenantResponse.Body.String(), "SETTLEMENT_INVALID_REQUEST") {
		t.Fatalf("cross-tenant contract event status=%d body=%s, want 400 SETTLEMENT_INVALID_REQUEST", crossTenantResponse.Code, crossTenantResponse.Body.String())
	}

	sameTenant := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", jsonBody(t, event(acceptedEventID, tenant)))
	sameTenant.Header.Set("Authorization", "Bearer e2e-machine-secret")
	sameTenantResponse := httptest.NewRecorder()
	handler.ServeHTTP(sameTenantResponse, sameTenant)
	if sameTenantResponse.Code != http.StatusAccepted {
		t.Fatalf("same-tenant contract event status=%d body=%s, want 202", sameTenantResponse.Code, sameTenantResponse.Body.String())
	}
	var envelope struct {
		Data struct {
			Accepted bool `json:"accepted"`
		} `json:"data"`
	}
	if err := json.NewDecoder(sameTenantResponse.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Data.Accepted {
		t.Fatalf("same-tenant contract event accepted=false, want true")
	}
	var snapshotCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_contract_snapshot WHERE tenant_id=? AND source_event_id=?`, tenant, acceptedEventID).Scan(&snapshotCount); err != nil || snapshotCount != 1 {
		t.Fatalf("same-tenant snapshot count=%d err=%v, want exactly one financial snapshot", snapshotCount, err)
	}
}

func cleanupContractEvent(t *testing.T, db *sql.DB, tenant, source, eventID, contractID string) {
	t.Helper()
	var snapshotID string
	_ = db.QueryRow(`SELECT id FROM settlement_contract_snapshot WHERE tenant_id=? AND source_event_id=?`, tenant, eventID).Scan(&snapshotID)
	if snapshotID != "" {
		if _, err := db.Exec(`DELETE FROM settlement_receivable_plan WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID); err != nil {
			t.Errorf("cleanup receivable plans: %v", err)
		}
		if _, err := db.Exec(`DELETE FROM settlement_contract_snapshot WHERE tenant_id=? AND id=?`, tenant, snapshotID); err != nil {
			t.Errorf("cleanup contract snapshot: %v", err)
		}
	}
	if _, err := db.Exec(`DELETE FROM settlement_inbox_event WHERE tenant_id=? AND source_application=? AND event_id=?`, tenant, source, eventID); err != nil {
		t.Errorf("cleanup inbox event: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM settlement_contract_stream WHERE tenant_id=? AND source_application=? AND source_contract_id=?`, tenant, source, contractID); err != nil {
		t.Errorf("cleanup contract stream: %v", err)
	}
}
