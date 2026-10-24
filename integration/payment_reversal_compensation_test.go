package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/service"
)

// 这些数据库测试需显式启用，并要求一次性 Settlement
// 数据库。验证结清 → 冲销 → 再结清生命周期，因为
// 补偿事实按应收结清轮次定位，而非仅按被冲销的
// 分配定位。

func openCreditCycleTest(t *testing.T, name string) (*sql.DB, context.Context, string, service.Principal, service.Principal, *service.Service, string) {
	t.Helper()
	if os.Getenv("SETTLEMENT_E2E_TEST") != "1" {
		t.Skip("set SETTLEMENT_E2E_TEST=1 to run the database-backed credit compensation flow")
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
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if err = db.PingContext(ctx); err != nil {
		t.Fatalf("database is not reachable: %v", err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	tenant := "e2e-credit-" + name + "-" + suffix
	principal := service.Principal{TenantID: tenant, UserID: "e2e-finance", Permissions: map[string]bool{"settlement.admin": true}}
	reverser := service.Principal{TenantID: tenant, UserID: "e2e-reverser", Permissions: map[string]bool{"settlement.admin": true}}
	publisher := &recordingCreditPublisher{}
	svc := &service.Service{DB: db, InvoiceIssuanceMode: "manual", CreditPublisher: publisher}
	return db, ctx, tenant, principal, reverser, svc, suffix
}

func creditCycleReceipt(t *testing.T, ctx context.Context, svc *service.Service, p service.Principal, amount, suffix string) string {
	t.Helper()
	id, err := svc.CreateReceipt(ctx, p, "cycle-receipt-"+suffix, service.ReceiptInput{
		CustomerID: "42", CustomerName: "E2E 客户", Amount: amount, Currency: "CNY",
		ReceiptDate: "2026-10-18", PaymentMethod: "TRANSFER", BankReference: "CYCLE-BANK-" + suffix,
	})
	if err != nil {
		t.Fatalf("create receipt %s: %v", amount, err)
	}
	return id
}

func creditCycleAllocate(t *testing.T, ctx context.Context, svc *service.Service, p service.Principal, receiptID, receivableID, amount, key string) string {
	t.Helper()
	id, err := svc.ConfirmAllocation(ctx, p, key, service.AllocationInput{ReceiptID: receiptID, ReceivableID: receivableID, Amount: amount, MatchMode: "MANUAL"})
	if err != nil {
		t.Fatalf("allocate %s: %v", amount, err)
	}
	return id
}

func assertCreditReceivable(t *testing.T, ctx context.Context, db *sql.DB, tenant, receivableID, wantStatus, wantReceived, wantOpen string) {
	t.Helper()
	var status, received, open string
	if err := db.QueryRowContext(ctx, `SELECT collection_status,received_allocated_amount,open_amount FROM settlement_receivable WHERE tenant_id=? AND id=?`, tenant, receivableID).Scan(&status, &received, &open); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || received != wantReceived || open != wantOpen {
		t.Fatalf("receivable status/received/open=%s/%s/%s, want %s/%s/%s", status, received, open, wantStatus, wantReceived, wantOpen)
	}
}

func creditCompletionEvent(t *testing.T, ctx context.Context, db *sql.DB, tenant, receivableID string) (eventID, allocationID string) {
	t.Helper()
	if err := db.QueryRowContext(ctx, `SELECT completed.event_id,completed.allocation_id FROM settlement_credit_sync completed WHERE completed.tenant_id=? AND completed.receivable_id=? AND completed.event_type=? AND completed.status<>'CANCELLED' AND NOT EXISTS (SELECT 1 FROM settlement_credit_sync compensating WHERE compensating.tenant_id=completed.tenant_id AND compensating.event_type=? AND compensating.original_event_id=completed.event_id) ORDER BY completed.created_at DESC,completed.id DESC LIMIT 1`, tenant, receivableID, "customer.credit.payment.completed.v1", "customer.credit.payment.reversed.v1").Scan(&eventID, &allocationID); err != nil {
		t.Fatalf("find completion event: %v", err)
	}
	return eventID, allocationID
}

func creditReversalEvent(t *testing.T, ctx context.Context, db *sql.DB, tenant, receivableID, originalEventID string) (status, payload string, found bool) {
	t.Helper()
	err := db.QueryRowContext(ctx, `SELECT status,payload_json FROM settlement_credit_sync WHERE tenant_id=? AND receivable_id=? AND event_type=? AND original_event_id=?`, tenant, receivableID, "customer.credit.payment.reversed.v1", originalEventID).Scan(&status, &payload)
	if err == sql.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return status, payload, true
}

func countCreditReversals(t *testing.T, ctx context.Context, db *sql.DB, tenant, receivableID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_credit_sync WHERE tenant_id=? AND receivable_id=? AND event_type=?`, tenant, receivableID, "customer.credit.payment.reversed.v1").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func decodeCreditPayload(t *testing.T, payload string) map[string]any {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestSettlementReversalCompensatesCompletionByReceivableCycle(t *testing.T) {
	db, ctx, tenant, principal, reverser, svc, suffix := openCreditCycleTest(t, "cycle")
	receivableID, _ := createReceivable(t, ctx, svc, principal, "CNY", "cycle-"+suffix)
	receiptA := creditCycleReceipt(t, ctx, svc, principal, "100.00", "A-"+suffix)
	allocationA := creditCycleAllocate(t, ctx, svc, principal, receiptA, receivableID, "50.00", "cycle-alloc-a-"+suffix)
	allocationB := creditCycleAllocate(t, ctx, svc, principal, receiptA, receivableID, "50.00", "cycle-alloc-b-"+suffix)
	assertCreditReceivable(t, ctx, db, tenant, receivableID, "SETTLED", "100.00", "0.00")
	event1, settledAllocation1 := creditCompletionEvent(t, ctx, db, tenant, receivableID)
	if settledAllocation1 != allocationB {
		t.Fatalf("completion event belongs to allocation %s, want final allocation %s", settledAllocation1, allocationB)
	}

	// 冲销较早分配使该期变为未结清。完成
	// 事实属于最后一个分配，因此补偿必须跟随
	// 应收轮次，而非被冲销分配 ID。
	reversal1, err := svc.ReverseAllocation(ctx, reverser, "cycle-reverse-1-"+suffix, allocationA, service.ReversalInput{Amount: "10.00", ReasonCode: "E2E_EARLY"})
	if err != nil {
		t.Fatalf("reverse early allocation: %v", err)
	}
	assertCreditReceivable(t, ctx, db, tenant, receivableID, "PARTIALLY_SETTLED", "90.00", "10.00")
	var completionStatus string
	if err = db.QueryRowContext(ctx, `SELECT status FROM settlement_credit_sync WHERE tenant_id=? AND event_id=?`, tenant, event1).Scan(&completionStatus); err != nil {
		t.Fatal(err)
	}
	reversalStatus1, reversalPayload1, found := creditReversalEvent(t, ctx, db, tenant, receivableID, event1)
	if !found || completionStatus != "DELIVERED" || reversalStatus1 != "DELIVERED" {
		t.Fatalf("completion=%s reversal=%v/%s", completionStatus, found, reversalStatus1)
	}
	reversalEvent1 := decodeCreditPayload(t, reversalPayload1)
	if reversalEvent1["reversal_of_payment_id"] != settledAllocation1 || reversalEvent1["paid_amount"] != "0.00" || reversalEvent1["customer_id"] != float64(42) {
		t.Fatalf("reversal payload=%v", reversalEvent1)
	}
	if countCreditReversals(t, ctx, db, tenant, receivableID) != 1 {
		t.Fatalf("expected exactly one compensation for completion %s", event1)
	}
	// 重放相同冲销请求必须返回既有冲销，
	// 不得产生第二个补偿事件。
	replay, err := svc.ReverseAllocation(ctx, reverser, "cycle-reverse-1-"+suffix, allocationA, service.ReversalInput{Amount: "10.00", ReasonCode: "E2E_EARLY"})
	if err != nil || replay != reversal1 {
		t.Fatalf("replayed reversal id=%q want=%q err=%v", replay, reversal1, err)
	}
	if countCreditReversals(t, ctx, db, tenant, receivableID) != 1 {
		t.Fatal("replayed reversal emitted another compensation event")
	}

	// 以新分配重新结清已打开的期间，创建新的
	// 完成事实。
	receiptC := creditCycleReceipt(t, ctx, svc, principal, "10.00", "C-"+suffix)
	allocationC := creditCycleAllocate(t, ctx, svc, principal, receiptC, receivableID, "10.00", "cycle-alloc-c-"+suffix)
	assertCreditReceivable(t, ctx, db, tenant, receivableID, "SETTLED", "100.00", "0.00")
	event2, settledAllocation2 := creditCompletionEvent(t, ctx, db, tenant, receivableID)
	if event2 == event1 || settledAllocation2 != allocationC {
		t.Fatalf("second completion event=%s allocation=%s, want new event on %s", event2, settledAllocation2, allocationC)
	}

	// 期间已结清时再次冲销较早分配。必须补偿新的
	// 完成轮次；已补偿事件不能
	// 重复补偿。
	if _, err = svc.ReverseAllocation(ctx, reverser, "cycle-reverse-2-"+suffix, allocationA, service.ReversalInput{Amount: "20.00", ReasonCode: "E2E_EARLY_AGAIN"}); err != nil {
		t.Fatalf("reverse early allocation after re-settlement: %v", err)
	}
	assertCreditReceivable(t, ctx, db, tenant, receivableID, "PARTIALLY_SETTLED", "80.00", "20.00")
	reversalStatus2, reversalPayload2, found := creditReversalEvent(t, ctx, db, tenant, receivableID, event2)
	if !found || reversalStatus2 != "DELIVERED" {
		t.Fatalf("second completion was not compensated: found=%v status=%s", found, reversalStatus2)
	}
	reversalEvent2 := decodeCreditPayload(t, reversalPayload2)
	if reversalEvent2["reversal_of_payment_id"] != settledAllocation2 {
		t.Fatalf("second reversal payload=%v, want reversal_of_payment_id=%s", reversalEvent2, settledAllocation2)
	}
	if countCreditReversals(t, ctx, db, tenant, receivableID) != 2 {
		t.Fatalf("expected one compensation per completion cycle, got %d", countCreditReversals(t, ctx, db, tenant, receivableID))
	}

	// 期间已未结清时进一步冲销，不得产生
	// 第三次补偿。
	if _, err = svc.ReverseAllocation(ctx, reverser, "cycle-reverse-3-"+suffix, allocationA, service.ReversalInput{Amount: "20.00", ReasonCode: "E2E_FINAL"}); err != nil {
		t.Fatalf("final early allocation reversal: %v", err)
	}
	assertCreditReceivable(t, ctx, db, tenant, receivableID, "PARTIALLY_SETTLED", "60.00", "40.00")
	if countCreditReversals(t, ctx, db, tenant, receivableID) != 2 {
		t.Fatal("unsettled period emitted an extra compensation event")
	}
}

func TestSettlementReversalCancelsUndeliveredCompletion(t *testing.T) {
	db, ctx, tenant, principal, reverser, _, suffix := openCreditCycleTest(t, "undelivered")
	// 有意不提供 CreditPublisher，使完成事实保持 PENDING。
	svc := &service.Service{DB: db}
	receivableID, _ := createReceivable(t, ctx, svc, principal, "CNY", "undelivered-"+suffix)
	receiptID := creditCycleReceipt(t, ctx, svc, principal, "100.00", "U-"+suffix)
	allocationID := creditCycleAllocate(t, ctx, svc, principal, receiptID, receivableID, "100.00", "undelivered-alloc-"+suffix)
	event, _ := creditCompletionEvent(t, ctx, db, tenant, receivableID)
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM settlement_credit_sync WHERE tenant_id=? AND event_id=?`, tenant, event).Scan(&status); err != nil || status != "PENDING" {
		t.Fatalf("completion status=%s err=%v, want PENDING", status, err)
	}
	if _, err := svc.ReverseAllocation(ctx, reverser, "undelivered-reverse-"+suffix, allocationID, service.ReversalInput{Amount: "100.00", ReasonCode: "E2E_UNDELIVERED"}); err != nil {
		t.Fatalf("reverse undelivered completion: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM settlement_credit_sync WHERE tenant_id=? AND event_id=?`, tenant, event).Scan(&status); err != nil || status != "CANCELLED" {
		t.Fatalf("completion status=%s err=%v, want CANCELLED", status, err)
	}
	if countCreditReversals(t, ctx, db, tenant, receivableID) != 0 {
		t.Fatal("undelivered completion must be cancelled, not compensated by a reversal event")
	}
	assertCreditReceivable(t, ctx, db, tenant, receivableID, "UNPAID", "0.00", "100.00")
}
