package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	notificationoutbox "github.com/j-s-te/settlement/internal/outbox"
	"strings"
	"testing"
	"time"
)

// importCommandFixture 构造独占测试合同的两期历史PREPARE命令。
// 参数：无。
// 返回值：仅用于隔离库的固定财务来源命令。
func importCommandFixture() ContractImportCommand {
	return ContractImportCommand{ExpectedFacts: []ExpectedImportFact{{SourceSystem: "legacy-ledger", FactType: "RECEIPT", ExternalFactID: "BANK-1"}, {SourceSystem: "legacy-ledger", FactType: "ALLOCATION", ExternalFactID: "ALLOC-1"}}, EventID: newID(), EventType: "contract.import_financial.v1", SchemaVersion: 1, Phase: "PREPARE", TenantID: "import-test", OriginImportItemID: newID(), SourceRevision: 1, PolicyDigest: strings.Repeat("a", 64), ReleaseVersion: 1, ImportMode: "CONTINUE_LIVE", OccurredAt: time.Now().UTC(), Contract: json.RawMessage(`{"id":"source-contract","no":"EXT-1","version":1,"customer_id":"42","customer_name":"历史客户","owner_user_id":"owner","currency":"CNY","amount":"100.00","payment_terms":[{"installment_no":1,"due_date":"2020-01-01","amount":"60.00"},{"installment_no":2,"due_date":"2020-02-01","amount":"40.00"}]}`)}
}

// TestImportFinancialFactsResume 验证提交成功后丢弃响应、服务重建及异内容不能重复财务事实。
// 参数：t为测试上下文，仅使用显式隔离MySQL。
// 返回值：无，凭证、核销、余额或来源映射不一致即失败。
func TestImportFinancialFactsResume(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	s := &Service{DB: db, ContractImportEnabled: true}
	in := importCommandFixture()
	prepared, err := s.PrepareContractImport(ctx, in.TenantID, in)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Status != "FINANCE_PREPARED" || prepared.MigrationState != "HOLD" {
		t.Fatal(prepared)
	}
	if duplicate, e := s.PrepareContractImport(ctx, in.TenantID, in); e != nil || !duplicate.Duplicate || duplicate.ContractSnapshotID != prepared.ContractSnapshotID {
		t.Fatalf("prepare replay=%+v err=%v", duplicate, e)
	}
	changed := in
	changed.PolicyDigest = strings.Repeat("b", 64)
	if _, e := s.PrepareContractImport(ctx, in.TenantID, changed); !errors.Is(e, ErrConflict) {
		t.Fatalf("different PREPARE=%v", e)
	}
	p := Principal{TenantID: in.TenantID, UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	var plan, receivable string
	if err = db.QueryRow(`SELECT id FROM settlement_receivable_plan WHERE contract_snapshot_id=? AND installment_no=1`, prepared.ContractSnapshotID).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmPlan(ctx, p, plan, PlanConfirmationInput{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT id FROM settlement_receivable WHERE receivable_plan_id=?`, plan).Scan(&receivable); err != nil {
		t.Fatal(err)
	}
	receipt := ReceiptInput{ImportFact: &ImportFact{ContractSnapshotID: prepared.ContractSnapshotID, SourceSystem: "legacy-ledger", FactType: "RECEIPT", ExternalFactID: "BANK-1"}, CustomerID: "42", CustomerName: "历史客户", Amount: "60.00", Currency: "CNY", ReceiptDate: "2020-01-03", PaymentMethod: "TRANSFER", BankReference: "BANK-1"}
	first, err := s.CreateReceipt(ctx, p, "ignored", receipt)
	if err != nil {
		t.Fatal(err)
	}
	s = &Service{DB: db, ContractImportEnabled: false}
	if duplicate, e := s.PrepareContractImport(ctx, in.TenantID, in); e != nil || !duplicate.Duplicate {
		t.Fatalf("disabled existing PREPARE resume=%+v err=%v", duplicate, e)
	}
	again, err := s.CreateReceipt(ctx, p, "new-network-request", receipt)
	if err != nil || again != first {
		t.Fatalf("receipt resume=%s %v", again, err)
	}
	allocation := AllocationInput{ImportFact: &ImportFact{ContractSnapshotID: prepared.ContractSnapshotID, SourceSystem: "legacy-ledger", FactType: "ALLOCATION", ExternalFactID: "ALLOC-1", OriginalReceiptFactID: "BANK-1", OriginalInstallmentNo: 1, OriginalAllocationID: "OLD-ALLOC-1"}, ReceiptID: first, ReceivableID: receivable, Amount: "60.00", MatchMode: "MANUAL"}
	original, err := s.ConfirmAllocation(ctx, p, "ignored", allocation)
	if err != nil {
		t.Fatal(err)
	}
	s = &Service{DB: db}
	again, err = s.ConfirmAllocation(ctx, p, "retry-after-restart", allocation)
	if err != nil || again != original {
		t.Fatalf("allocation resume=%s %v", again, err)
	}
	receipt.Amount = "61.00"
	if _, e := s.CreateReceipt(ctx, p, "changed", receipt); !errors.Is(e, ErrConflict) {
		t.Fatalf("same fact changed content=%v", e)
	}
	allocation.Amount = "59.00"
	if _, e := s.ConfirmAllocation(ctx, p, "changed", allocation); !errors.Is(e, ErrConflict) {
		t.Fatalf("same allocation changed content=%v", e)
	}
	var count int
	var open string
	if err = db.QueryRow(`SELECT COUNT(*) FROM settlement_receipt`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipts=%d %v", count, err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM settlement_receipt_allocation`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("allocations=%d %v", count, err)
	}
	if err = db.QueryRow(`SELECT open_amount FROM settlement_receivable WHERE id=?`, receivable).Scan(&open); err != nil || open != "0.00" {
		t.Fatalf("open=%s %v", open, err)
	}
}

// TestImportedFinanceHoldNoNotificationsOrCredit 验证历史结清只产生HOLD信用事件，重启及手动重试均不投递。
// 参数：t为测试上下文。
// 返回值：无，发生任何CRM投递时失败。
func TestImportedFinanceHoldNoNotificationsOrCredit(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	s := &Service{DB: db, ContractImportEnabled: true}
	in := importCommandFixture()
	in.ExpectedFacts = []ExpectedImportFact{{SourceSystem: "legacy", FactType: "RECEIPT", ExternalFactID: "r1"}, {SourceSystem: "legacy", FactType: "ALLOCATION", ExternalFactID: "a1"}}
	prepared, err := s.PrepareContractImport(ctx, in.TenantID, in)
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{TenantID: in.TenantID, UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	var plan, receivable string
	db.QueryRow(`SELECT id FROM settlement_receivable_plan WHERE contract_snapshot_id=? AND installment_no=1`, prepared.ContractSnapshotID).Scan(&plan)
	if err = s.ConfirmPlan(ctx, p, plan, PlanConfirmationInput{Version: 1}); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT id FROM settlement_receivable WHERE receivable_plan_id=?`, plan).Scan(&receivable)
	receipt, err := s.CreateReceipt(ctx, p, "receipt", ReceiptInput{ImportFact: &ImportFact{ContractSnapshotID: prepared.ContractSnapshotID, SourceSystem: "legacy", FactType: "RECEIPT", ExternalFactID: "r1"}, CustomerID: "42", CustomerName: "客户", Amount: "60", Currency: "CNY", ReceiptDate: "2020-01-01", PaymentMethod: "TRANSFER", BankReference: "bank1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ConfirmAllocation(ctx, p, "allocation", AllocationInput{ImportFact: &ImportFact{ContractSnapshotID: prepared.ContractSnapshotID, SourceSystem: "legacy", FactType: "ALLOCATION", ExternalFactID: "a1", OriginalReceiptFactID: "r1", OriginalInstallmentNo: 1, OriginalAllocationID: "a1"}, ReceiptID: receipt, ReceivableID: receivable, Amount: "60", MatchMode: "MANUAL"})
	if err != nil {
		t.Fatal(err)
	}
	var status, event string
	if err = db.QueryRow(`SELECT status,event_id FROM settlement_credit_sync`).Scan(&status, &event); err != nil || status != "HOLD" {
		t.Fatalf("historical credit status=%s err=%v", status, err)
	}
	// 模拟旧进程已积压PENDING信用和待发通知，重启后的发送器必须再核对HOLD。
	if _, err = db.Exec(`UPDATE settlement_credit_sync SET status='PENDING'`); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"reference_type": "RECEIVABLE", "reference_id": receivable})
	if _, err = db.Exec(`INSERT INTO settlement_outbox_event(id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,status,available_at,created_at,updated_at) VALUES(?,?,?,'PLATFORM_NOTIFICATION','SETTLEMENT_RECEIVABLE_OVERDUE','receivable',?,?,'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newID(), p.TenantID, newID(), receivable, payload); err != nil {
		t.Fatal(err)
	}
	notificationWorker := notificationoutbox.Worker{Store: &notificationoutbox.Store{DB: db, WorkerID: "restart", Lease: time.Minute}, Destinations: []notificationoutbox.Destination{{Name: "PLATFORM_NOTIFICATION"}}, BatchSize: 10}
	if err = notificationWorker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var pendingStatus string
	if err = db.QueryRow(`SELECT status FROM settlement_outbox_event WHERE destination='PLATFORM_NOTIFICATION'`).Scan(&pendingStatus); err != nil || pendingStatus != "CANCELLED" {
		t.Fatalf("pending notification status=%s err=%v", pendingStatus, err)
	}
	overview, err := s.DunningOverview(ctx, p)
	if err != nil || overview.OverdueCount != 0 || overview.ActiveCaseCount != 0 || overview.ActionCount != 0 {
		t.Fatalf("HOLD dunning overview=%+v err=%v", overview, err)
	}
	aging, err := s.AgingSummary(ctx, p)
	if err != nil || len(aging) != 0 {
		t.Fatalf("HOLD aging=%+v err=%v", aging, err)
	}
	publisher := &fakeCreditPublisher{}
	for i := 0; i < 2; i++ {
		if err = (&CreditSyncWorker{DB: db, Publisher: publisher}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	s.CreditPublisher = publisher
	if err = s.RetryCreditSync(ctx, p.TenantID, event); !errors.Is(err, ErrNotFound) {
		t.Fatalf("manual retry=%v", err)
	}
	if len(publisher.events) != 0 {
		t.Fatalf("HOLD credit delivered %v", publisher.events)
	}
}

// TestReleaseReconcilesOutstandingOnly 验证逐期余额、责任ACK和本地核对凭证共同约束独立放行。
// 参数：t为测试上下文。
// 返回值：无，旧核对、错误责任版本、重复放行或信用决议错误时失败。
func TestReleaseReconcilesOutstandingOnly(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	s := &Service{DB: db, ContractImportEnabled: true}
	in := importCommandFixture()
	prepared, err := s.PrepareContractImport(ctx, in.TenantID, in)
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{TenantID: in.TenantID, UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	rows, err := db.Query(`SELECT id FROM settlement_receivable_plan WHERE contract_snapshot_id=? ORDER BY installment_no`, prepared.ContractSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var plans []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, id)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, plan := range plans {
		if err = s.ConfirmPlan(ctx, p, plan, PlanConfirmationInput{Version: 1}); err != nil {
			t.Fatal(err)
		}
	}
	statement := ImportReconcileInput{Currency: "CNY", ExpectedOpenAmount: "40.00", ExpectedReceiptAmount: "60.00", ExpectedInvoicedAmount: "0.00", CreditDecision: "SUPPRESSED", Reason: "经核对历史结清不回灌信用"}
	if _, err = s.ReconcileContractImport(ctx, p, prepared.ContractSnapshotID, statement); !errors.Is(err, ErrImportBlocked) {
		t.Fatalf("incomplete facts released: %v", err)
	}
	var receivable string
	if err = db.QueryRow(`SELECT id FROM settlement_receivable WHERE receivable_plan_id=?`, plans[0]).Scan(&receivable); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.CreateReceipt(ctx, p, "receipt", ReceiptInput{ImportFact: &ImportFact{ContractSnapshotID: prepared.ContractSnapshotID, SourceSystem: "legacy-ledger", FactType: "RECEIPT", ExternalFactID: "BANK-1"}, CustomerID: "42", CustomerName: "历史客户", Amount: "60", Currency: "CNY", ReceiptDate: "2020-01-03", PaymentMethod: "TRANSFER", BankReference: "bank1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ConfirmAllocation(ctx, p, "allocation", AllocationInput{ImportFact: &ImportFact{ContractSnapshotID: prepared.ContractSnapshotID, SourceSystem: "legacy-ledger", FactType: "ALLOCATION", ExternalFactID: "ALLOC-1", OriginalReceiptFactID: "BANK-1", OriginalInstallmentNo: 1, OriginalAllocationID: "OLD-ALLOC-1"}, ReceiptID: receipt, ReceivableID: receivable, Amount: "60", MatchMode: "MANUAL"})
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := s.ReconcileContractImport(ctx, p, prepared.ContractSnapshotID, statement)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.OpenAmount != "40.00" || reconciled.AllocatedAmount != "60.00" || reconciled.FactCount != 2 {
		t.Fatalf("balances=%+v", reconciled)
	}
	release := in
	release.EventID = newID()
	release.Phase = "RELEASE"
	release.Contract = nil
	release.ExpectedFacts = nil
	release.ReconciliationID = reconciled.ID
	release.ExpectedSnapshotVersion = reconciled.SnapshotVersion
	release.ExpectedOwnerVersion = 2
	if _, err = s.ReleaseContractImport(ctx, in.TenantID, prepared.ContractSnapshotID, release); !errors.Is(err, ErrConflict) {
		t.Fatalf("release without owner ACK=%v", err)
	}
	_, err = s.IngestContractResponsibility(ctx, in.TenantID, ContractResponsibilityEvent{EventID: newID(), EventType: "contract.responsibility_changed.v1", TenantID: in.TenantID, OccurredAt: time.Now().UTC(), SourceContractID: "source-contract", SourceVersion: 2, OwnerUserID: "owner", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	// 后续核对审批替换旧凭证，即使金额未变也不能用旧信用决定放行。
	newer, err := s.ReconcileContractImport(ctx, p, prepared.ContractSnapshotID, statement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleaseContractImport(ctx, in.TenantID, prepared.ContractSnapshotID, release); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale reconciliation=%v", err)
	}
	release.ReconciliationID = newer.ID
	// 与并发恢复共用快照锁：放行必须等待恢复提交，再拒绝已失效核对版本。
	recovery, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = lockImportWrite(ctx, recovery, p, prepared.ContractSnapshotID); err != nil {
		recovery.Rollback()
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		_, e := s.ReleaseContractImport(ctx, in.TenantID, prepared.ContractSnapshotID, release)
		released <- e
	}()
	select {
	case e := <-released:
		recovery.Rollback()
		t.Fatalf("release crossed recovery snapshot lock: %v", e)
	case <-time.After(100 * time.Millisecond):
	}
	if err = recovery.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-released; !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent recovery did not invalidate release: %v", err)
	}
	current, err := s.ReconcileContractImport(ctx, p, prepared.ContractSnapshotID, statement)
	if err != nil {
		t.Fatal(err)
	}
	release.ReconciliationID = current.ID
	release.ExpectedSnapshotVersion = current.SnapshotVersion
	s = &Service{DB: db, ContractImportEnabled: false}
	result, err := s.ReleaseContractImport(ctx, in.TenantID, prepared.ContractSnapshotID, release)
	if err != nil || result.Status != "FINANCE_RELEASED" {
		t.Fatalf("release=%+v %v", result, err)
	}
	duplicate, err := s.ReleaseContractImport(ctx, in.TenantID, prepared.ContractSnapshotID, release)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("release replay=%+v %v", duplicate, err)
	}
	var state, credit string
	var open string
	if err = db.QueryRow(`SELECT migration_state FROM settlement_contract_snapshot WHERE id=?`, prepared.ContractSnapshotID).Scan(&state); err != nil || state != "RELEASED" {
		t.Fatalf("state=%s err=%v", state, err)
	}
	if err = db.QueryRow(`SELECT status FROM settlement_credit_sync`).Scan(&credit); err != nil || credit != "SUPPRESSED" {
		t.Fatalf("credit=%s err=%v", credit, err)
	}
	if err = db.QueryRow(`SELECT SUM(open_amount) FROM settlement_receivable`).Scan(&open); err != nil || open != "40.00" {
		t.Fatalf("outstanding=%s err=%v", open, err)
	}
}

// TestUnsupportedImportFactsBlocked 验证旧票、退款与冲销在任何业务写入前明确阻塞。
// 参数：t为测试上下文。
// 返回值：无，误受理或退回普通模式即失败。
func TestUnsupportedImportFactsBlocked(t *testing.T) {
	for _, kind := range []string{"INVOICE", "RED_INVOICE", "REFUND", "REVERSAL"} {
		in := importCommandFixture()
		in.ExpectedFacts = []ExpectedImportFact{{SourceSystem: "legacy", FactType: kind, ExternalFactID: "old1"}}
		_, err := (&Service{ContractImportEnabled: true}).PrepareContractImport(context.Background(), in.TenantID, in)
		if !errors.Is(err, ErrImportBlocked) {
			t.Fatalf("kind=%s err=%v", kind, err)
		}
	}
}

// TestImportExportBeyondTenThousand 验证完整快照导出超过一万行仍保留总数和金额，并可查询失败。
// 参数：t为隔离MySQL测试上下文。
// 返回值：无，截断、总额不一致、失败冒充文件或进程中断不可查询时失败。
func TestImportExportBeyondTenThousand(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	s := &Service{DB: db, ContractImportEnabled: true}
	in := importCommandFixture()
	in.ExpectedFacts = []ExpectedImportFact{}
	prepared, err := s.PrepareContractImport(ctx, in.TenantID, in)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 10001; i++ {
		id := fmt.Sprintf("%032d", i+1)
		fact := fmt.Sprintf("BANK-%05d", i+1)
		_, err = tx.Exec(`INSERT INTO settlement_receipt(id,tenant_id,receipt_no,customer_id,customer_name_snapshot,amount,unallocated_amount,currency,receipt_date,payment_method,bank_transaction_reference,source_type,status,idempotency_key,created_at,updated_at) VALUES(?,? ,?,'42','历史客户',1,1,'CNY','2020-01-01','TRANSFER',?,'MANUAL','AVAILABLE',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, in.TenantID, fact, fact, fact)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(`INSERT INTO settlement_import_fact(tenant_id,contract_snapshot_id,source_system,fact_type,external_fact_id,request_hash,target_business_id,created_at) VALUES(?,?,'legacy','RECEIPT',?,REPEAT('a',32),?,UTC_TIMESTAMP(3))`, in.TenantID, prepared.ContractSnapshotID, fact, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	p := Principal{TenantID: in.TenantID, UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	exported, err := s.CreateImportExport(ctx, p, prepared.ContractSnapshotID)
	if err != nil || exported.Status != "SUCCEEDED" || exported.RowCount != 10001 || exported.ReceiptTotal != "10001.00" {
		t.Fatalf("export=%+v err=%v", exported, err)
	}
	loaded, err := s.GetImportExport(ctx, p, prepared.ContractSnapshotID, exported.ID)
	if err != nil {
		t.Fatal(err)
	}
	csvRows, err := csv.NewReader(bytes.NewReader(loaded.Content)).ReadAll()
	if err != nil || len(csvRows) != 10004 {
		t.Fatalf("csv rows=%d err=%v", len(csvRows), err)
	}
	// 模拟来源映射缺失业务凭证，导出必须持久失败且不能下载空文件。
	_, err = db.Exec(`INSERT INTO settlement_import_fact(tenant_id,contract_snapshot_id,source_system,fact_type,external_fact_id,request_hash,target_business_id,created_at) VALUES(?,?,'legacy','RECEIPT','MISSING',REPEAT('a',32),'missing',UTC_TIMESTAMP(3))`, in.TenantID, prepared.ContractSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.CreateImportExport(ctx, p, prepared.ContractSnapshotID)
	if err != nil || failed.Status != "FAILED" {
		t.Fatalf("failed export=%+v err=%v", failed, err)
	}
	queried, err := s.GetImportExport(ctx, p, prepared.ContractSnapshotID, failed.ID)
	if err != nil || queried.Status != "FAILED" || len(queried.Content) != 0 {
		t.Fatalf("failure query=%+v err=%v", queried, err)
	}
	if _, err = db.Exec(`UPDATE settlement_import_export SET status='PENDING',error_code='',created_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 10 MINUTE) WHERE id=?`, failed.ID); err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.GetImportExport(ctx, p, prepared.ContractSnapshotID, failed.ID)
	if err != nil || interrupted.Status != "FAILED" || interrupted.ErrorCode != "EXPORT_PROCESS_INTERRUPTED" {
		t.Fatalf("interrupted export=%+v err=%v", interrupted, err)
	}

}
