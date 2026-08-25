package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
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
	"github.com/j-s-te/settlement/internal/service"
)

// TestInvoiceFlow proves the business path behind the invoice receivable
// selector. It deliberately does not insert business records directly:
// contract data enters through IngestContract, the receivable is created by
// ConfirmPlan, and the invoice reservation enters through the HTTP API.
//
// The test is opt-in because it writes to the database. Run it only against a
// disposable or dedicated Settlement database:
//
// SETTLEMENT_E2E_TEST=1 SETTLEMENT_E2E_TEST_DSN='...' go test ./integration -v
func TestInvoiceFlow(t *testing.T) {
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
	source := "integration-test"
	suffix := time.Now().UTC().Format("20060102150405.000000000")
	eventID := "E2E-INVOICE-" + strings.ReplaceAll(suffix, ".", "")
	contractID := "E2E-CONTRACT-" + strings.ReplaceAll(suffix, ".", "")
	contractNo := "E2E-HT-" + strings.ReplaceAll(suffix, ".", "")
	defer cleanupInvoiceFlow(t, db, tenant, source, eventID, contractID)

	event := service.ContractEvent{
		EventID:    eventID,
		EventType:  "contract.financial_effective.v1",
		TenantID:   tenant,
		OccurredAt: time.Now().UTC(),
	}
	event.Aggregate.ID = contractID
	event.Aggregate.Version = 1
	event.Contract = struct {
		ID           string                `json:"id"`
		No           string                `json:"no"`
		Version      int                   `json:"version"`
		CustomerID   string                `json:"customer_id"`
		CustomerName string                `json:"customer_name"`
		ProjectID    string                `json:"project_id"`
		ProjectName  string                `json:"project_name"`
		Currency     string                `json:"currency"`
		Amount       string                `json:"amount"`
		PaymentTerms []service.PaymentTerm `json:"payment_terms"`
	}{
		ID: contractID, No: contractNo, Version: 1,
		CustomerID: "customer-e2e", CustomerName: "集成测试客户",
		ProjectID: "project-e2e", ProjectName: "集成测试项目",
		Currency: "CNY", Amount: "120000.00",
		PaymentTerms: []service.PaymentTerm{{InstallmentNo: 1, DueDate: "2099-12-31", Amount: "120000.00"}},
	}

	svc := &service.Service{DB: db}
	accepted, err := svc.IngestContract(ctx, source, event)
	if err != nil || !accepted {
		t.Fatalf("ingest contract: accepted=%v err=%v", accepted, err)
	}

	var snapshotID, planID string
	var planVersion int
	if err := db.QueryRowContext(ctx, `SELECT cs.id,rp.id,rp.version FROM settlement_contract_snapshot cs JOIN settlement_receivable_plan rp ON rp.contract_snapshot_id=cs.id WHERE cs.tenant_id=? AND cs.source_event_id=?`, tenant, eventID).Scan(&snapshotID, &planID, &planVersion); err != nil {
		t.Fatalf("find pending receivable plan: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("receivable must not exist before confirmation: count=%d err=%v", count, err)
	}

	if err := svc.ConfirmPlan(ctx, service.Principal{TenantID: tenant, UserID: "dev-finance", Permissions: map[string]bool{"settlement.admin": true}}, planID, planVersion); err != nil {
		t.Fatalf("confirm receivable plan: %v", err)
	}
	var receivableID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID).Scan(&receivableID); err != nil {
		t.Fatalf("find confirmed receivable: %v", err)
	}

	handler := httpapi.New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "http://localhost:5173"}, slog.Default(), nil, nil)
	assertEligibleAmount(t, handler, "120000.00", receivableID)

	firstRequest := createInvoiceRequest(t, handler, snapshotID, receivableID, "50000.00", "e2e-invoice-1")
	if firstRequest == "" {
		t.Fatal("first invoice request did not return an id")
	}
	// Reusing the key is safe and returns the existing request instead of
	// reserving another 50,000 yuan.
	if got := createInvoiceRequest(t, handler, snapshotID, receivableID, "50000.00", "e2e-invoice-1"); got != firstRequest {
		t.Fatalf("idempotent retry returned %q, want %q", got, firstRequest)
	}
	assertEligibleAmount(t, handler, "70000.00", receivableID)

	secondRequest := createInvoiceRequest(t, handler, snapshotID, receivableID, "70000.00", "e2e-invoice-2")
	if secondRequest == "" || secondRequest == firstRequest {
		t.Fatalf("second invoice request id=%q is invalid", secondRequest)
	}
	assertNoEligibleReceivable(t, handler)

	// The backend must reject any amount beyond the confirmed and unreserved
	// balance, even if a caller bypasses the UI.
	assertInvoiceRequestStatus(t, handler, snapshotID, receivableID, "1.00", "e2e-invoice-over-limit", http.StatusConflict)
}

func createInvoiceRequest(t *testing.T, handler http.Handler, snapshotID, receivableID, amount, key string) string {
	t.Helper()
	payload := map[string]any{
		"contract_snapshot_id": snapshotID,
		"buyer_profile":        map[string]any{"name": "集成测试客户", "tax_no": "91330000E2E"},
		"invoice_type":         "VAT_SPECIAL",
		"items":                []map[string]string{{"item_name": "集成测试服务", "quantity": "1", "unit_price_excl_tax": amount, "amount_excl_tax": amount, "tax_rate": "0", "tax_amount": "0", "amount_incl_tax": amount}},
		"allocations":          []map[string]string{{"receivable_id": receivableID, "reserved_amount": amount}},
	}
	return invoiceRequest(t, handler, payload, key, http.StatusCreated)
}

func assertInvoiceRequestStatus(t *testing.T, handler http.Handler, snapshotID, receivableID, amount, key string, want int) {
	t.Helper()
	payload := map[string]any{
		"contract_snapshot_id": snapshotID,
		"buyer_profile":        map[string]any{"name": "集成测试客户"}, "invoice_type": "VAT_SPECIAL",
		"items":       []map[string]string{{"item_name": "超额测试", "quantity": "1", "unit_price_excl_tax": amount, "amount_excl_tax": amount, "tax_rate": "0", "tax_amount": "0", "amount_incl_tax": amount}},
		"allocations": []map[string]string{{"receivable_id": receivableID, "reserved_amount": amount}},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-requests", jsonBody(t, payload))
	request.Header.Set("X-CSRF-Token", "1")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("over-limit invoice status=%d body=%s, want %d", response.Code, response.Body.String(), want)
	}
}

func invoiceRequest(t *testing.T, handler http.Handler, payload map[string]any, key string, want int) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-requests", jsonBody(t, payload))
	request.Header.Set("X-CSRF-Token", "1")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("invoice request status=%d body=%s, want %d", response.Code, response.Body.String(), want)
	}
	var envelope struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data.ID
}

func assertEligibleAmount(t *testing.T, handler http.Handler, want, receivableID string) {
	t.Helper()
	items := eligibleReceivables(t, handler)
	if len(items) != 1 || items[0]["id"] != receivableID || items[0]["invoiceable_amount"] != want {
		t.Fatalf("eligible receivables=%v, want one %s with invoiceable_amount=%s", items, receivableID, want)
	}
}

func assertNoEligibleReceivable(t *testing.T, handler http.Handler) {
	t.Helper()
	if items := eligibleReceivables(t, handler); len(items) != 0 {
		t.Fatalf("eligible receivables after full reservation=%v, want empty", items)
	}
}

func eligibleReceivables(t *testing.T, handler http.Handler) []map[string]string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/invoice-eligible-receivables", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("eligible receivables status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func jsonBody(t *testing.T, value any) io.Reader {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(body)
}

func cleanupInvoiceFlow(t *testing.T, db *sql.DB, tenant, source, eventID, contractID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Errorf("begin cleanup: %v", err)
		return
	}
	defer tx.Rollback()
	var snapshotID string
	_ = tx.QueryRowContext(ctx, `SELECT id FROM settlement_contract_snapshot WHERE tenant_id=? AND source_event_id=?`, tenant, eventID).Scan(&snapshotID)
	var receivableIDs []string
	if snapshotID != "" {
		rows, queryErr := tx.QueryContext(ctx, `SELECT id FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID)
		if queryErr == nil {
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					receivableIDs = append(receivableIDs, id)
				}
			}
			rows.Close()
		}
	}
	for _, id := range receivableIDs {
		_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_request_allocation WHERE tenant_id=? AND receivable_id=?`, tenant, id)
	}
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_request_item WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?)`, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_outbox_event WHERE tenant_id=? AND aggregate_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?)`, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_receivable_plan WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_contract_snapshot WHERE tenant_id=? AND id=?`, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_inbox_event WHERE tenant_id=? AND source_application=? AND event_id=?`, tenant, source, eventID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_contract_stream WHERE tenant_id=? AND source_application=? AND source_contract_id=?`, tenant, source, contractID)
	if err := tx.Commit(); err != nil {
		t.Errorf("cleanup invoice flow: %v", err)
	}
}
