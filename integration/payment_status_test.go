package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/service"
)

// TestPaymentStatusAndManualInvoiceCurrency 在一次性
// Settlement MySQL 数据库运行，验证 MySQL 基于已更新行
// 求值的余额和状态更新，以及复制到人工发票的
// 合同币种。
func TestPaymentStatusAndManualInvoiceCurrency(t *testing.T) {
	if os.Getenv("SETTLEMENT_E2E_TEST") != "1" {
		t.Skip("set SETTLEMENT_E2E_TEST=1 to run the database-backed payment flow")
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
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("database is not reachable: %v", err)
	}
	publisher := &recordingCreditPublisher{}
	svc := &service.Service{DB: db, InvoiceIssuanceMode: "manual", CreditPublisher: publisher}
	principal := service.Principal{TenantID: "e2e-payment", UserID: "e2e-finance", Permissions: map[string]bool{"settlement.admin": true}}
	reverser := service.Principal{TenantID: principal.TenantID, UserID: "e2e-reverser", Permissions: map[string]bool{"settlement.admin": true}}
	suffix := time.Now().UTC().Format("150405000000")

	writeoffReceivable, writeoffSnapshot := createReceivable(t, ctx, svc, principal, "CNY", "writeoff-"+suffix)
	receiptID, err := svc.CreateReceipt(ctx, principal, "e2e-receipt-"+suffix, service.ReceiptInput{
		CustomerID: "42", CustomerName: "E2E 客户", Amount: "100.00", Currency: "CNY",
		ReceiptDate: "2026-10-18", PaymentMethod: "TRANSFER", BankReference: "E2E-BANK-" + suffix,
	})
	if err != nil {
		t.Fatalf("create receipt: %v", err)
	}
	firstAllocation, err := svc.ConfirmAllocation(ctx, principal, "e2e-alloc-1-"+suffix, service.AllocationInput{
		ReceiptID: receiptID, ReceivableID: writeoffReceivable, Amount: "50.00", MatchMode: "MANUAL",
	})
	if err != nil {
		t.Fatalf("first allocation: %v", err)
	}
	assertReceiptAndReceivable(t, ctx, db, principal.TenantID, receiptID, writeoffReceivable, "PARTIALLY_ALLOCATED", "50.00", "PARTIALLY_SETTLED", "50.00", "50.00")
	if _, err = svc.ConfirmAllocation(ctx, principal, "e2e-alloc-2-"+suffix, service.AllocationInput{
		ReceiptID: receiptID, ReceivableID: writeoffReceivable, Amount: "50.00", MatchMode: "MANUAL",
	}); err != nil {
		t.Fatalf("second allocation: %v", err)
	}
	assertReceiptAndReceivable(t, ctx, db, principal.TenantID, receiptID, writeoffReceivable, "FULLY_ALLOCATED", "0.00", "SETTLED", "100.00", "0.00")

	if _, err = svc.ReverseAllocation(ctx, reverser, "e2e-reverse-1-"+suffix, firstAllocation, service.ReversalInput{Amount: "30.00", ReasonCode: "E2E"}); err != nil {
		t.Fatalf("partial reversal: %v", err)
	}
	assertReceiptAndReceivable(t, ctx, db, principal.TenantID, receiptID, writeoffReceivable, "PARTIALLY_ALLOCATED", "30.00", "PARTIALLY_SETTLED", "70.00", "30.00")
	if _, err = svc.ReverseAllocation(ctx, reverser, "e2e-reverse-2-"+suffix, firstAllocation, service.ReversalInput{Amount: "20.00", ReasonCode: "E2E"}); err != nil {
		t.Fatalf("complete first reversal: %v", err)
	}
	assertReceiptAndReceivable(t, ctx, db, principal.TenantID, receiptID, writeoffReceivable, "PARTIALLY_ALLOCATED", "50.00", "PARTIALLY_SETTLED", "50.00", "50.00")

	// 一次性结清也必须进入完全结清状态。
	fullReceivable, _ := createReceivable(t, ctx, svc, principal, "CNY", "full-"+suffix)
	fullReceiptID, err := svc.CreateReceipt(ctx, principal, "e2e-full-receipt-"+suffix, service.ReceiptInput{
		CustomerID: "42", CustomerName: "E2E 客户", Amount: "100.00", Currency: "CNY",
		ReceiptDate: "2026-10-18", PaymentMethod: "TRANSFER", BankReference: "E2E-FULL-BANK-" + suffix,
	})
	if err != nil {
		t.Fatalf("create full receipt: %v", err)
	}
	fullAllocationID, err := svc.ConfirmAllocation(ctx, principal, "e2e-full-alloc-"+suffix, service.AllocationInput{
		ReceiptID: fullReceiptID, ReceivableID: fullReceivable, Amount: "100.00", MatchMode: "MANUAL",
	})
	if err != nil {
		t.Fatalf("full allocation: %v", err)
	}
	assertReceiptAndReceivable(t, ctx, db, principal.TenantID, fullReceiptID, fullReceivable, "FULLY_ALLOCATED", "0.00", "SETTLED", "100.00", "0.00")
	var creditPayload []byte
	if err = db.QueryRowContext(ctx, `SELECT payload_json FROM settlement_credit_sync WHERE tenant_id=? AND allocation_id=?`, principal.TenantID, fullAllocationID).Scan(&creditPayload); err != nil {
		t.Fatalf("find credit sync payload: %v", err)
	}
	var credit struct {
		EventType  string `json:"event_type"`
		CustomerID uint64 `json:"customer_id"`
		PeriodNo   string `json:"period_no"`
		DueDate    string `json:"due_date"`
		PaidDate   string `json:"paid_date"`
		DueAmount  string `json:"due_amount"`
		PaidAmount string `json:"paid_amount"`
	}
	if err = json.Unmarshal(creditPayload, &credit); err != nil {
		t.Fatal(err)
	}
	if credit.EventType != "customer.credit.payment.completed.v1" || credit.CustomerID != 42 || credit.PeriodNo != "1" || credit.DueDate != "2099-12-31T00:00:00Z" || credit.PaidDate != "2026-10-18T00:00:00Z" || credit.DueAmount != "100.00" || credit.PaidAmount != "100.00" {
		t.Fatalf("credit payload=%+v", credit)
	}
	if _, err = svc.ReverseAllocation(ctx, reverser, "e2e-full-reverse-"+suffix, fullAllocationID, service.ReversalInput{Amount: "100.00", ReasonCode: "E2E_REVERSAL"}); err != nil {
		t.Fatalf("full reversal: %v", err)
	}
	assertReceiptAndReceivable(t, ctx, db, principal.TenantID, fullReceiptID, fullReceivable, "AVAILABLE", "100.00", "UNPAID", "0.00", "100.00")
	var completionStatus, reversalStatus, reversalOriginal, completionPayloadJSON, reversalPayloadJSON string
	if err = db.QueryRowContext(ctx, `SELECT status,payload_json FROM settlement_credit_sync WHERE tenant_id=? AND allocation_id=? AND event_type=?`, principal.TenantID, fullAllocationID, "customer.credit.payment.completed.v1").Scan(&completionStatus, &completionPayloadJSON); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT status,original_event_id,payload_json FROM settlement_credit_sync WHERE tenant_id=? AND allocation_id=? AND event_type=?`, principal.TenantID, fullAllocationID, "customer.credit.payment.reversed.v1").Scan(&reversalStatus, &reversalOriginal, &reversalPayloadJSON); err != nil {
		t.Fatal(err)
	}
	if completionStatus != "DELIVERED" || reversalStatus != "DELIVERED" || reversalOriginal == "" {
		t.Fatalf("credit sync completion=%s reversal=%s original=%s", completionStatus, reversalStatus, reversalOriginal)
	}
	var reversalPayload struct {
		EventType           string `json:"event_type"`
		ReversalOfPaymentID string `json:"reversal_of_payment_id"`
		PaidAmount          string `json:"paid_amount"`
		CustomerID          uint64 `json:"customer_id"`
	}
	if err = json.Unmarshal([]byte(reversalPayloadJSON), &reversalPayload); err != nil {
		t.Fatal(err)
	}
	if reversalPayload.EventType != "customer.credit.payment.reversed.v1" || reversalPayload.ReversalOfPaymentID != fullAllocationID || reversalPayload.PaidAmount != "0.00" || reversalPayload.CustomerID != 42 {
		t.Fatalf("reversal payload=%+v", reversalPayload)
	}

	// 独立 USD 合同验证人工发票继承合同
	// 币种，部分已开票余额不会跳为 FULLY_INVOICED。
	invoiceReceivable, invoiceSnapshot := createReceivable(t, ctx, svc, principal, "USD", "invoice-"+suffix)
	manualInvoice(t, ctx, db, svc, principal, invoiceSnapshot, invoiceReceivable, "60.00", "USD", "60.00", "PARTIALLY_INVOICED", "e2e-invoice-1-"+suffix)
	manualInvoice(t, ctx, db, svc, principal, invoiceSnapshot, invoiceReceivable, "30.00", "USD", "90.00", "PARTIALLY_INVOICED", "e2e-invoice-2-"+suffix)
	manualInvoice(t, ctx, db, svc, principal, invoiceSnapshot, invoiceReceivable, "10.00", "USD", "100.00", "FULLY_INVOICED", "e2e-invoice-3-"+suffix)
	t.Logf("writeoff snapshot=%s invoice snapshot=%s", writeoffSnapshot, invoiceSnapshot)
}

func createReceivable(t *testing.T, ctx context.Context, svc *service.Service, p service.Principal, currency, suffix string) (receivableID, snapshotID string) {
	t.Helper()
	event := service.ContractEvent{
		EventID: "E2E-EVENT-" + strings.ToUpper(suffix), EventType: "contract.financial_effective.v1",
		TenantID: p.TenantID, OccurredAt: time.Now().UTC(),
	}
	event.Aggregate.ID = "E2E-CONTRACT-" + suffix
	event.Aggregate.Version = 1
	event.Contract.ID = event.Aggregate.ID
	event.Contract.No = "E2E-NO-" + suffix
	event.Contract.Version = 1
	event.Contract.CustomerID = "42"
	event.Contract.CustomerName = "E2E 客户"
	event.Contract.ProjectID = "e2e-project"
	event.Contract.ProjectName = "E2E 项目"
	event.Contract.Currency = currency
	event.Contract.Amount = "100.00"
	event.Contract.PaymentTerms = []service.PaymentTerm{{InstallmentNo: 1, DueDate: "2099-12-31", Amount: "100.00"}}
	accepted, err := svc.IngestContract(ctx, "e2e-payment-test", p.TenantID, event)
	if err != nil || !accepted {
		t.Fatalf("ingest contract: accepted=%v err=%v", accepted, err)
	}
	var planID string
	var planVersion int
	if err = svc.DB.QueryRowContext(ctx, `SELECT cs.id,rp.id,rp.version FROM settlement_contract_snapshot cs JOIN settlement_receivable_plan rp ON rp.contract_snapshot_id=cs.id WHERE cs.tenant_id=? AND cs.source_event_id=?`, p.TenantID, event.EventID).Scan(&snapshotID, &planID, &planVersion); err != nil {
		t.Fatal(err)
	}
	if err = svc.ConfirmPlan(ctx, p, planID, service.PlanConfirmationInput{Version: planVersion}); err != nil {
		t.Fatalf("confirm plan: %v", err)
	}
	if err = svc.DB.QueryRowContext(ctx, `SELECT id FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=?`, p.TenantID, snapshotID).Scan(&receivableID); err != nil {
		t.Fatal(err)
	}
	return receivableID, snapshotID
}

func assertReceiptAndReceivable(t *testing.T, ctx context.Context, db *sql.DB, tenant, receiptID, receivableID, receiptStatus, unallocated, collectionStatus, received, open string) {
	t.Helper()
	var gotReceiptStatus, gotUnallocated, gotCollectionStatus, gotReceived, gotOpen string
	if err := db.QueryRowContext(ctx, `SELECT status,unallocated_amount FROM settlement_receipt WHERE tenant_id=? AND id=?`, tenant, receiptID).Scan(&gotReceiptStatus, &gotUnallocated); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT collection_status,received_allocated_amount,open_amount FROM settlement_receivable WHERE tenant_id=? AND id=?`, tenant, receivableID).Scan(&gotCollectionStatus, &gotReceived, &gotOpen); err != nil {
		t.Fatal(err)
	}
	if gotReceiptStatus != receiptStatus || gotUnallocated != unallocated || gotCollectionStatus != collectionStatus || gotReceived != received || gotOpen != open {
		t.Fatalf("receipt=(%s,%s) receivable=(%s,%s,%s), want (%s,%s)/(%s,%s,%s)", gotReceiptStatus, gotUnallocated, gotCollectionStatus, gotReceived, gotOpen, receiptStatus, unallocated, collectionStatus, received, open)
	}
}

func manualInvoice(t *testing.T, ctx context.Context, db *sql.DB, svc *service.Service, p service.Principal, snapshotID, receivableID, amount, wantCurrency, wantInvoiced, wantStatus, key string) {
	t.Helper()
	requestID, err := svc.CreateInvoiceRequest(ctx, p, key, service.InvoiceRequestInput{
		ContractSnapshotID: snapshotID,
		BuyerProfile:       map[string]any{"name": "E2E 客户"},
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
	if _, err = svc.ApproveInvoiceRequest(ctx, service.Principal{TenantID: p.TenantID, UserID: "e2e-reviewer", Permissions: map[string]bool{"settlement.admin": true}}, requestID, version); err != nil {
		t.Fatalf("approve invoice request %s: %v", amount, err)
	}
	if _, err = svc.RegisterManualInvoice(ctx, service.Principal{TenantID: p.TenantID, UserID: "e2e-issuer", Permissions: map[string]bool{"settlement.admin": true}}, requestID, service.ManualInvoiceInput{InvoiceCode: "E2E", InvoiceNo: "E2E-" + requestID[:12], IssueDate: "2026-10-18"}); err != nil {
		t.Fatalf("register manual invoice %s: %v", amount, err)
	}
	var currency, invoiced, status string
	if err = db.QueryRowContext(ctx, `SELECT currency FROM settlement_tax_invoice WHERE tenant_id=? AND invoice_request_id=?`, p.TenantID, requestID).Scan(&currency); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT invoiced_amount,invoice_status FROM settlement_receivable WHERE tenant_id=? AND id=?`, p.TenantID, receivableID).Scan(&invoiced, &status); err != nil {
		t.Fatal(err)
	}
	if currency != wantCurrency || invoiced != wantInvoiced || status != wantStatus {
		t.Fatalf("invoice %s currency=%s invoiced=%s status=%s, want %s/%s/%s", amount, currency, invoiced, status, wantCurrency, wantInvoiced, wantStatus)
	}
}

type recordingCreditPublisher struct {
	events []string
}

func (p *recordingCreditPublisher) Publish(_ context.Context, _ string, eventID string, _ []byte) error {
	p.events = append(p.events, eventID)
	return nil
}
