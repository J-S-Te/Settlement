package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestImportAllocationCannotCrossSnapshot 验证来源A不能核销已放行的合同B。
// 参数：t为隔离MySQL测试上下文。
// 返回值：无；跨快照写入或错误映射未回滚时失败。
func TestImportAllocationCannotCrossSnapshot(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	s := &Service{DB: db, ContractImportEnabled: true}
	a := importCommandFixture()
	first, e := s.PrepareContractImport(ctx, a.TenantID, a)
	if e != nil {
		t.Fatal(e)
	}
	b := importCommandFixture()
	var body map[string]any
	if e = json.Unmarshal(b.Contract, &body); e != nil {
		t.Fatal(e)
	}
	body["id"] = "contract-B"
	body["no"] = "B"
	b.Contract, _ = json.Marshal(body)
	second, e := s.PrepareContractImport(ctx, b.TenantID, b)
	if e != nil {
		t.Fatal(e)
	}
	p := Principal{TenantID: a.TenantID, UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	var plan, receivable string
	if e = db.QueryRow(`SELECT id FROM settlement_receivable_plan WHERE contract_snapshot_id=? AND installment_no=1`, second.ContractSnapshotID).Scan(&plan); e != nil {
		t.Fatal(e)
	}
	if e = s.ConfirmPlan(ctx, p, plan, PlanConfirmationInput{Version: 1}); e != nil {
		t.Fatal(e)
	}
	if e = db.QueryRow(`SELECT id FROM settlement_receivable WHERE receivable_plan_id=?`, plan).Scan(&receivable); e != nil {
		t.Fatal(e)
	}
	receipt, e := s.CreateReceipt(ctx, p, "B-receipt", ReceiptInput{ImportFact: &ImportFact{ContractSnapshotID: second.ContractSnapshotID, SourceSystem: "legacy-ledger", FactType: "RECEIPT", ExternalFactID: "BANK-1"}, CustomerID: "42", CustomerName: "历史客户", Amount: "60", Currency: "CNY", ReceiptDate: "2020-01-03", PaymentMethod: "TRANSFER", BankReference: "B-bank"})
	if e != nil {
		t.Fatal(e)
	}
	// 仅测试夹具构造已放行B，检验所有实际目标状态的来源约束。
	if _, e = db.Exec(`UPDATE settlement_contract_snapshot SET migration_state='RELEASED' WHERE id=?`, second.ContractSnapshotID); e != nil {
		t.Fatal(e)
	}
	_, e = s.ConfirmAllocation(ctx, p, "cross-snapshot", AllocationInput{ImportFact: &ImportFact{ContractSnapshotID: first.ContractSnapshotID, SourceSystem: "legacy-ledger", FactType: "ALLOCATION", ExternalFactID: "ALLOC-1", OriginalReceiptFactID: "BANK-1", OriginalInstallmentNo: 1, OriginalAllocationID: "old-1"}, ReceiptID: receipt, ReceivableID: receivable, Amount: "60", MatchMode: "MANUAL"})
	if !errors.Is(e, ErrInvalid) && !errors.Is(e, ErrForbidden) {
		t.Fatalf("跨快照核销未被拒绝: %v", e)
	}
	var count int
	if e = db.QueryRow(`SELECT COUNT(*) FROM settlement_import_fact WHERE contract_snapshot_id=? AND fact_type='ALLOCATION'`, first.ContractSnapshotID).Scan(&count); e != nil || count != 0 {
		t.Fatalf("失败保留事实映射: count=%d error=%v", count, e)
	}
}

// TestImportFactIdentityPreservesCase 验证外部事实键按来源精确区分大小写。
// 参数：t为隔离MySQL测试上下文。
// 返回值：无；合法不同键被合并或尾空格被接受时失败。
func TestImportFactIdentityPreservesCase(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	s := &Service{DB: db, ContractImportEnabled: true}
	in := importCommandFixture()
	in.ExpectedFacts = []ExpectedImportFact{{SourceSystem: "legacy", FactType: "RECEIPT", ExternalFactID: "BANK-A"}, {SourceSystem: "legacy", FactType: "RECEIPT", ExternalFactID: "bank-a"}}
	result, e := s.PrepareContractImport(ctx, in.TenantID, in)
	if e != nil {
		t.Fatal(e)
	}
	p := Principal{TenantID: in.TenantID, UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	for _, key := range []string{"BANK-A", "bank-a"} {
		_, e = s.CreateReceipt(ctx, p, key, ReceiptInput{ImportFact: &ImportFact{ContractSnapshotID: result.ContractSnapshotID, SourceSystem: "legacy", FactType: "RECEIPT", ExternalFactID: key}, CustomerID: "42", CustomerName: "历史客户", Amount: "10", Currency: "CNY", ReceiptDate: "2020-01-03", PaymentMethod: "TRANSFER", BankReference: "proof-" + newID()})
		if e != nil {
			t.Fatalf("原事实键%s被错误合并: %v", key, e)
		}
	}
	in.EventID = newID()
	in.OriginImportItemID = newID()
	in.ExpectedFacts[0].ExternalFactID = "BANK-A "
	if _, e = s.PrepareContractImport(ctx, in.TenantID, in); !errors.Is(e, ErrInvalid) {
		t.Fatalf("非规范事实键未拒绝: %v", e)
	}
}
