package integration_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/service"
)

// TestPartialTaxInvoiceAndPartialRedFlush 验证外部税控回调
// 路径：同一应收分两次部分开票，之后仅红冲第二次。
// 剩余已开票余额必须保持部分状态，不能跳到全额
// 或零。
func TestPartialTaxInvoiceAndPartialRedFlush(t *testing.T) {
	if os.Getenv("SETTLEMENT_E2E_TEST") != "1" {
		t.Skip("set SETTLEMENT_E2E_TEST=1 to run the database-backed tax callback flow")
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		t.Fatalf("database is not reachable: %v", err)
	}
	tenant := "e2e-tax-partial"
	principal := service.Principal{TenantID: tenant, UserID: "e2e-finance", Permissions: map[string]bool{"settlement.admin": true}}
	svc := &service.Service{DB: db, InvoiceIssuanceMode: "tax_adapter", TaxProviderCode: "e2e-provider"}
	suffix := time.Now().UTC().Format("150405000000")
	receivableID, snapshotID := createReceivable(t, ctx, svc, principal, "CNY", "pt"+suffix)

	firstInvoice := issueTaxInvoice(t, ctx, db, svc, principal, snapshotID, receivableID, "60.00", "PARTIAL-1-"+suffix)
	secondInvoice := issueTaxInvoice(t, ctx, db, svc, principal, snapshotID, receivableID, "30.00", "PARTIAL-2-"+suffix)
	_ = firstInvoice
	assertInvoicedState(t, ctx, db, tenant, receivableID, "90.00", "PARTIALLY_INVOICED")

	redRequestID, err := svc.RequestInvoiceRedFlush(ctx, principal, "red-request-"+suffix, secondInvoice, service.InvoiceRedFlushInput{ReasonCode: "INVOICE_ERROR", ReasonDetail: "E2E partial red flush"})
	if err != nil {
		t.Fatalf("request red flush: %v", err)
	}
	if err = svc.ReviewInvoiceRedFlush(ctx, service.Principal{TenantID: tenant, UserID: "e2e-reviewer", Permissions: map[string]bool{"settlement.admin": true}}, redRequestID, true, service.InvoiceRedFlushReviewInput{Version: 1}); err != nil {
		t.Fatalf("approve red flush: %v", err)
	}
	var redAttemptID string
	if err = db.QueryRowContext(ctx, `SELECT id FROM settlement_invoice_red_flush_attempt WHERE tenant_id=? AND red_flush_request_id=?`, tenant, redRequestID).Scan(&redAttemptID); err != nil {
		t.Fatal(err)
	}
	result := taxResult("30.00", "RED-"+suffix, "RED-NO-"+suffix, "RED-EXT-"+suffix)
	redEvent := service.TaxCallbackEvent{
		EventID: "tax-red-" + suffix, EventType: service.TaxInvoiceRedFlushedEvent, TenantID: tenant,
		ProviderCode: "e2e-provider", OccurredAt: time.Now().UTC(), ResultSequence: 1,
		RedFlushAttemptID: redAttemptID, RedFlushRequestID: redRequestID,
		ExternalRequestID: redAttemptID, ExternalOperationID: "RED-OP-" + suffix, Invoice: result,
	}
	if applied, callbackErr := svc.ApplyTaxCallback(ctx, tenant, redEvent); callbackErr != nil || applied != "APPLIED" {
		t.Fatalf("apply partial red flush: result=%q err=%v", applied, callbackErr)
	}
	assertInvoicedState(t, ctx, db, tenant, receivableID, "60.00", "PARTIALLY_INVOICED")
}

func issueTaxInvoice(t *testing.T, ctx context.Context, db *sql.DB, svc *service.Service, p service.Principal, snapshotID, receivableID, amount, suffix string) string {
	t.Helper()
	requestID, err := svc.CreateInvoiceRequest(ctx, p, "issue-"+suffix, service.InvoiceRequestInput{
		ContractSnapshotID: snapshotID,
		BuyerProfile:       map[string]any{"name": "E2E 客户", "tax_no": "91330000E2E"},
		InvoiceType:        "VAT_SPECIAL",
		Items: []service.InvoiceItemInput{{
			ItemName: "E2E service", Quantity: "1", UnitPriceExclTax: amount,
			AmountExclTax: amount, TaxRate: "0", TaxAmount: "0", AmountInclTax: amount,
		}},
		Allocations: []service.InvoiceAllocationInput{{ReceivableID: receivableID, ReservedAmount: amount}},
	})
	if err != nil {
		t.Fatalf("create invoice request %s: %v", amount, err)
	}
	var version int
	if err = db.QueryRowContext(ctx, `SELECT version FROM settlement_invoice_request WHERE tenant_id=? AND id=?`, p.TenantID, requestID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	attemptID, err := svc.ApproveInvoiceRequest(ctx, service.Principal{TenantID: p.TenantID, UserID: "e2e-reviewer", Permissions: map[string]bool{"settlement.admin": true}}, requestID, version)
	if err != nil {
		t.Fatalf("approve invoice request %s: %v", amount, err)
	}
	result := taxResult(amount, "CODE-"+suffix, "NO-"+suffix, "EXT-"+suffix)
	event := service.TaxCallbackEvent{
		EventID: "tax-issued-" + suffix, EventType: service.TaxInvoiceIssuedEvent, TenantID: p.TenantID,
		ProviderCode: "e2e-provider", OccurredAt: time.Now().UTC(), ResultSequence: 1,
		AttemptID: attemptID, ExternalRequestID: attemptID, ExternalOperationID: "OP-" + suffix, Invoice: result,
	}
	if applied, callbackErr := svc.ApplyTaxCallback(ctx, p.TenantID, event); callbackErr != nil || applied != "APPLIED" {
		t.Fatalf("apply tax issuance %s: result=%q err=%v", amount, applied, callbackErr)
	}
	var invoiceID string
	if err = db.QueryRowContext(ctx, `SELECT id FROM settlement_tax_invoice WHERE tenant_id=? AND invoice_request_id=?`, p.TenantID, requestID).Scan(&invoiceID); err != nil {
		t.Fatal(err)
	}
	return invoiceID
}

func taxResult(amount, code, no, externalID string) *service.TaxInvoiceResult {
	return &service.TaxInvoiceResult{
		ExternalInvoiceID: externalID, InvoiceCode: code, InvoiceNo: no, InvoiceType: "VAT_SPECIAL", Currency: "CNY",
		AmountExclTax: amount, TaxAmount: "0.00", AmountInclTax: amount, IssueDate: "2026-10-18",
		SellerProfile: map[string]any{"name": "E2E seller"},
		Items:         []service.InvoiceItemInput{{ItemName: "E2E service", Quantity: "1", UnitPriceExclTax: amount, AmountExclTax: amount, TaxRate: "0", TaxAmount: "0", AmountInclTax: amount}},
	}
}

func assertInvoicedState(t *testing.T, ctx context.Context, db *sql.DB, tenant, receivableID, wantAmount, wantStatus string) {
	t.Helper()
	var amount, status string
	if err := db.QueryRowContext(ctx, `SELECT invoiced_amount,invoice_status FROM settlement_receivable WHERE tenant_id=? AND id=?`, tenant, receivableID).Scan(&amount, &status); err != nil {
		t.Fatal(err)
	}
	if amount != wantAmount || status != wantStatus {
		t.Fatalf("invoiced amount/status=%s/%s, want %s/%s", amount, status, wantAmount, wantStatus)
	}
}
