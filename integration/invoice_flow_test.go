package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	eventID := "E2E-" + strings.ReplaceAll(suffix, ".", "")
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

	if err := svc.ConfirmPlan(ctx, service.Principal{TenantID: tenant, UserID: "dev-finance", Permissions: map[string]bool{"settlement.admin": true}}, planID, service.PlanConfirmationInput{Version: planVersion}); err != nil {
		t.Fatalf("confirm receivable plan: %v", err)
	}
	var receivableID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID).Scan(&receivableID); err != nil {
		t.Fatalf("find confirmed receivable: %v", err)
	}

	handler := httpapi.New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "http://localhost:5173"}, slog.Default(), nil, nil, nil)
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
	// A lost success response after the final reservation must still be safely
	// replayable even though no invoiceable balance remains.
	if got := createInvoiceRequest(t, handler, snapshotID, receivableID, "70000.00", "e2e-invoice-2"); got != secondRequest {
		t.Fatalf("full-balance idempotent retry returned %q, want %q", got, secondRequest)
	}
	assertInvoiceRequestStatus(t, handler, snapshotID, receivableID, "1.00", "e2e-invoice-2", http.StatusConflict)

	manualService := &service.Service{DB: db, InvoiceIssuanceMode: "manual"}
	if _, err := manualService.ApproveInvoiceRequest(ctx, service.Principal{TenantID: tenant, UserID: "reviewer-1"}, firstRequest, 1); err != nil {
		t.Fatalf("approve manual invoice request: %v", err)
	}
	assertIssueChannelAndTaxCommands(t, db, tenant, firstRequest, "MANUAL", 0)
	manualInvoiceID, err := manualService.RegisterManualInvoice(ctx, service.Principal{TenantID: tenant, UserID: "issuer-1"}, firstRequest, service.ManualInvoiceInput{InvoiceCode: "E2E-CODE", InvoiceNo: "E2E-NO-" + firstRequest[:8], IssueDate: time.Now().UTC().Format("2006-01-02")})
	if err != nil || manualInvoiceID == "" {
		t.Fatalf("register manual invoice: id=%q err=%v", manualInvoiceID, err)
	}
	if _, err = manualService.RegisterManualInvoice(ctx, service.Principal{TenantID: tenant, UserID: "issuer-1"}, firstRequest, service.ManualInvoiceInput{InvoiceCode: "E2E-CODE", InvoiceNo: "E2E-DUP-" + firstRequest[:8], IssueDate: time.Now().UTC().Format("2006-01-02")}); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("duplicate manual issue err=%v, want conflict", err)
	}

	adapterService := &service.Service{DB: db, InvoiceIssuanceMode: "tax_adapter", TaxProviderCode: "e2e-provider"}
	adapterAttemptID, err := adapterService.ApproveInvoiceRequest(ctx, service.Principal{TenantID: tenant, UserID: "reviewer-2"}, secondRequest, 1)
	if err != nil {
		t.Fatalf("approve adapter invoice request: %v", err)
	}
	assertIssueChannelAndTaxCommands(t, db, tenant, secondRequest, "TAX_ADAPTER", 1)
	if _, err = adapterService.RegisterManualInvoice(ctx, service.Principal{TenantID: tenant, UserID: "issuer-1"}, secondRequest, service.ManualInvoiceInput{InvoiceCode: "E2E-CODE", InvoiceNo: "E2E-ADAPTER-" + secondRequest[:8], IssueDate: time.Now().UTC().Format("2006-01-02")}); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("adapter request accepted manual invoice: %v", err)
	}
	unknown := service.TaxCallbackEvent{EventID: "tax-unknown-" + adapterAttemptID, EventType: service.TaxInvoiceIssueUnknownEvent, TenantID: tenant, ProviderCode: "e2e-provider", OccurredAt: time.Now().UTC(), ResultSequence: 1, AttemptID: adapterAttemptID, ExternalRequestID: adapterAttemptID, FailureCode: "TIMEOUT", FailureMessage: "result temporarily unknown"}
	if result, callbackErr := adapterService.ApplyTaxCallback(ctx, tenant, unknown); callbackErr != nil || result != "APPLIED" {
		t.Fatalf("apply unknown tax result: result=%q err=%v", result, callbackErr)
	}
	invoiceResult := &service.TaxInvoiceResult{ExternalInvoiceID: "EXT-" + adapterAttemptID, InvoiceCode: "E2E-TAX", InvoiceNo: "NO-" + adapterAttemptID[:12], InvoiceType: "VAT_SPECIAL", Currency: "CNY", AmountExclTax: "70000.00", TaxAmount: "0.00", AmountInclTax: "70000.00", IssueDate: time.Now().UTC().Format("2006-01-02"), SellerProfile: map[string]any{"name": "E2E seller"}, Items: []service.InvoiceItemInput{{ItemName: "集成测试服务", Quantity: "1", UnitPriceExclTax: "70000.00", AmountExclTax: "70000.00", TaxRate: "0", TaxAmount: "0", AmountInclTax: "70000.00"}}}
	issued := service.TaxCallbackEvent{EventID: "tax-issued-" + adapterAttemptID, EventType: service.TaxInvoiceIssuedEvent, TenantID: tenant, ProviderCode: "e2e-provider", OccurredAt: time.Now().UTC(), ResultSequence: 2, AttemptID: adapterAttemptID, ExternalRequestID: adapterAttemptID, ExternalOperationID: "OP-" + adapterAttemptID, Invoice: invoiceResult}
	if result, callbackErr := adapterService.ApplyTaxCallback(ctx, tenant, issued); callbackErr != nil || result != "APPLIED" {
		t.Fatalf("apply issued tax result: result=%q err=%v", result, callbackErr)
	}
	if result, callbackErr := adapterService.ApplyTaxCallback(ctx, tenant, issued); callbackErr != nil || result != "APPLIED" {
		t.Fatalf("duplicate issued tax result: result=%q err=%v", result, callbackErr)
	}
	conflicting := issued
	conflicting.FailureMessage = "tampered replay"
	if _, callbackErr := adapterService.ApplyTaxCallback(ctx, tenant, conflicting); !errors.Is(callbackErr, service.ErrConflict) {
		t.Fatalf("conflicting tax result err=%v, want conflict", callbackErr)
	}
	stale := unknown
	stale.EventID = "tax-stale-" + adapterAttemptID
	if result, callbackErr := adapterService.ApplyTaxCallback(ctx, tenant, stale); callbackErr != nil || result != "IGNORED_STALE" {
		t.Fatalf("stale tax result: result=%q err=%v", result, callbackErr)
	}
	taxHandler := httpapi.New(db, config.Config{DevelopmentAuth: true, TaxResultIngestEnabled: true, TaxResultBearerToken: "e2e-tax-secret", OIDCTenantID: tenant, PublicOrigin: "http://localhost:5173", TaxProviderCode: "e2e-provider"}, slog.Default(), nil, nil, nil)
	httpStale := stale
	httpStale.EventID = "tax-http-stale-" + adapterAttemptID
	unauthorized := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/tax-results", jsonBody(t, httpStale))
	unauthorizedResponse := httptest.NewRecorder()
	taxHandler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("tax callback without machine token status=%d, want 401", unauthorizedResponse.Code)
	}
	authorized := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/tax-results", jsonBody(t, httpStale))
	authorized.Header.Set("Authorization", "Bearer e2e-tax-secret")
	authorizedResponse := httptest.NewRecorder()
	taxHandler.ServeHTTP(authorizedResponse, authorized)
	if authorizedResponse.Code != http.StatusAccepted {
		t.Fatalf("tax callback with machine token status=%d body=%s, want 202", authorizedResponse.Code, authorizedResponse.Body.String())
	}
	var adapterInvoiceID string
	if err = db.QueryRowContext(ctx, `SELECT id FROM settlement_tax_invoice WHERE tenant_id=? AND invoice_request_id=? AND issued_by_channel='TAX_ADAPTER'`, tenant, secondRequest).Scan(&adapterInvoiceID); err != nil {
		t.Fatalf("find adapter invoice: %v", err)
	}
	redRequestID, err := adapterService.RequestInvoiceRedFlush(ctx, service.Principal{TenantID: tenant, UserID: "red-requester"}, "e2e-red-"+adapterInvoiceID, adapterInvoiceID, service.InvoiceRedFlushInput{ReasonCode: "INVOICE_ERROR", ReasonDetail: "E2E red flush"})
	if err != nil {
		t.Fatalf("request red flush: %v", err)
	}
	if err = adapterService.ReviewInvoiceRedFlush(ctx, service.Principal{TenantID: tenant, UserID: "red-reviewer"}, redRequestID, true, service.InvoiceRedFlushReviewInput{Version: 1}); err != nil {
		t.Fatalf("approve red flush: %v", err)
	}
	var redAttemptID string
	if err = db.QueryRowContext(ctx, `SELECT id FROM settlement_invoice_red_flush_attempt WHERE tenant_id=? AND red_flush_request_id=?`, tenant, redRequestID).Scan(&redAttemptID); err != nil {
		t.Fatalf("find red flush attempt: %v", err)
	}
	redResult := *invoiceResult
	redResult.ExternalInvoiceID = "RED-EXT-" + redAttemptID
	redResult.InvoiceCode = "E2E-RED"
	redResult.InvoiceNo = "RED-" + redAttemptID[:12]
	redIssued := service.TaxCallbackEvent{EventID: "tax-red-" + redAttemptID, EventType: service.TaxInvoiceRedFlushedEvent, TenantID: tenant, ProviderCode: "e2e-provider", OccurredAt: time.Now().UTC(), ResultSequence: 1, RedFlushAttemptID: redAttemptID, RedFlushRequestID: redRequestID, ExternalRequestID: redAttemptID, ExternalOperationID: "RED-OP-" + redAttemptID, Invoice: &redResult}
	if result, callbackErr := adapterService.ApplyTaxCallback(ctx, tenant, redIssued); callbackErr != nil || result != "APPLIED" {
		t.Fatalf("apply red tax result: result=%q err=%v", result, callbackErr)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_invoice_red_flush_relation WHERE tenant_id=? AND red_flush_request_id=?`, tenant, redRequestID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("red flush relation count=%d err=%v", count, err)
	}

	// The red invoice appends a reversal fact and restores only the red-flushed
	// 70,000 allocation; the manually issued 50,000 remains invoiced.
	assertEligibleAmount(t, handler, "70000.00", receivableID)
}

func assertIssueChannelAndTaxCommands(t *testing.T, db *sql.DB, tenant, requestID, wantChannel string, wantCommands int) {
	t.Helper()
	var channel string
	if err := db.QueryRow(`SELECT channel FROM settlement_invoice_issue_attempt WHERE tenant_id=? AND invoice_request_id=? ORDER BY attempt_no DESC LIMIT 1`, tenant, requestID).Scan(&channel); err != nil || channel != wantChannel {
		t.Fatalf("issue channel=%q err=%v, want %q", channel, err, wantChannel)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_outbox_event WHERE tenant_id=? AND destination='TAX_INVOICE_COMMAND' AND aggregate_id=?`, tenant, requestID).Scan(&count); err != nil || count != wantCommands {
		t.Fatalf("tax commands=%d err=%v, want %d", count, err, wantCommands)
	}
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

func eligibleReceivables(t *testing.T, handler http.Handler) []map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/invoice-eligible-receivables", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("eligible receivables status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data []map[string]any `json:"data"`
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
		_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_allocation_reversal WHERE tenant_id=? AND original_allocation_id IN (SELECT id FROM settlement_invoice_request_allocation WHERE tenant_id=? AND receivable_id=?)`, tenant, tenant, id)
	}
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_tax_result_inbox WHERE tenant_id=? AND aggregate_id IN (SELECT id FROM settlement_invoice_issue_attempt WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?) UNION SELECT id FROM settlement_invoice_red_flush_attempt WHERE tenant_id=? AND original_invoice_id IN (SELECT id FROM settlement_tax_invoice WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?)))`, tenant, tenant, tenant, snapshotID, tenant, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_red_flush_relation WHERE tenant_id=? AND original_invoice_id IN (SELECT id FROM settlement_tax_invoice WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?))`, tenant, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_red_flush_attempt WHERE tenant_id=? AND original_invoice_id IN (SELECT id FROM settlement_tax_invoice WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?))`, tenant, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_red_flush_request WHERE tenant_id=? AND original_invoice_id IN (SELECT id FROM settlement_tax_invoice WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?))`, tenant, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_document WHERE tenant_id=? AND tax_invoice_id IN (SELECT id FROM settlement_tax_invoice WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?))`, tenant, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_tax_invoice_item WHERE tenant_id=? AND tax_invoice_id IN (SELECT id FROM settlement_tax_invoice WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?))`, tenant, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET tax_invoice_id=NULL WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?)`, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_tax_invoice WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?)`, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_issue_attempt WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?)`, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_request_item WHERE tenant_id=? AND invoice_request_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?)`, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_outbox_event WHERE tenant_id=? AND aggregate_id IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?)`, tenant, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_outbox_event WHERE tenant_id=? AND aggregate_id NOT IN (SELECT id FROM settlement_invoice_request WHERE tenant_id=?) AND payload_json LIKE ?`, tenant, tenant, "%"+snapshotID+"%")
	for _, id := range receivableIDs {
		_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_request_allocation WHERE tenant_id=? AND receivable_id=?`, tenant, id)
	}
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_idempotency_record WHERE tenant_id=? AND command_type='CREATE_INVOICE_REQUEST' AND idempotency_key LIKE 'e2e-invoice-%'`, tenant)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_receivable_plan WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_contract_snapshot WHERE tenant_id=? AND id=?`, tenant, snapshotID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_inbox_event WHERE tenant_id=? AND source_application=? AND event_id=?`, tenant, source, eventID)
	_, _ = tx.ExecContext(ctx, `DELETE FROM settlement_contract_stream WHERE tenant_id=? AND source_application=? AND source_contract_id=?`, tenant, source, contractID)
	if err := tx.Commit(); err != nil {
		t.Errorf("cleanup invoice flow: %v", err)
	}
}
