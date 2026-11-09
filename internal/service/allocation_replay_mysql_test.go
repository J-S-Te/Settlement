package service

import (
	"errors"
	"testing"
)

// allocationReplayFixture 创建普通100元核销所需的真实计划、回款和可信财务主体。
// 参数：t为测试上下文，仅通过显式隔离DSN新建独占数据库。
// 返回值：服务、主体、应收、回款及普通核销输入。
func allocationReplayFixture(t *testing.T) (*Service, Principal, string, string, AllocationInput) {
	t.Helper()
	db, _ := invoiceArchiveMySQL(t)
	p := roleFlowPrincipal("settlement_accountant", "allocator", newID())
	svc := &Service{DB: db}
	svc.ConfigureFinancialSources(true, false)
	receivable, _ := seedCreditFlowPlan(t, db, p.TenantID)
	receipt, err := svc.CreateReceipt(t.Context(), p, newID(), ReceiptInput{
		CustomerID: "42", CustomerName: "隔离核销客户", Amount: "100", Currency: "CNY",
		ReceiptDate: "2026-11-01", PaymentMethod: "TRANSFER", BankReference: newID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	in := AllocationInput{ReceiptID: receipt, ReceivableID: receivable, Amount: "100.00", MatchMode: "MANUAL"}
	return svc, p, receivable, receipt, in
}

// TestMySQLAllocationReplaysBeforeTerminalBalances 验证原键规范同内容在核销终态返回首次标识。
// 参数：t为测试上下文，预期金额等价和模式空白归一化保持同一业务请求。
// 返回值：无；重复扣款、变更内容被接受或丢失原成功标识时失败。
func TestMySQLAllocationReplaysBeforeTerminalBalances(t *testing.T) {
	svc, p, receivable, receipt, in := allocationReplayFixture(t)
	key := newID()
	first, err := svc.ConfirmAllocation(t.Context(), p, key, in)
	if err != nil {
		t.Fatal(err)
	}
	replay := in
	replay.Amount = "100"
	replay.MatchMode = " manual "
	again, err := svc.ConfirmAllocation(t.Context(), p, key, replay)
	if err != nil || again != first {
		t.Fatalf("成功回放=%s，期望%s，err=%v", again, first, err)
	}
	for _, changed := range []AllocationInput{
		{ReceiptID: receipt, ReceivableID: receivable, Amount: "99", MatchMode: "MANUAL"},
		{ReceiptID: receipt, ReceivableID: receivable, Amount: "100", MatchMode: "SUGGESTED", MatchConfidence: 80},
	} {
		if _, err := svc.ConfirmAllocation(t.Context(), p, key, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("同键不同内容应冲突: %v", err)
		}
	}
	var count int
	if err := svc.DB.QueryRow(`SELECT COUNT(*) FROM settlement_receipt_allocation WHERE tenant_id=?`, p.TenantID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("核销数=%d err=%v", count, err)
	}
	assertCreditFlowState(t, svc.DB, p.TenantID, receivable, "100.00", "0.00", "SETTLED")
	assertDifferenceReceipt(t, svc.DB, p.TenantID, receipt, "0.00", "FULLY_ALLOCATED", 2)
}

// TestMySQLAllocationReplayKeepsAuthorization 验证回放仍核验当前操作人、租户、权限及范围。
// 参数：t为测试上下文，使用正式目录角色与实际终态核销。
// 返回值：无；无权主体得到原核销标识或跨操作人复用幂等键时失败。
func TestMySQLAllocationReplayKeepsAuthorization(t *testing.T) {
	svc, p, _, _, in := allocationReplayFixture(t)
	key := newID()
	if _, err := svc.ConfirmAllocation(t.Context(), p, key, in); err != nil {
		t.Fatal(err)
	}
	other := p
	other.UserID = "other-allocator"
	if _, err := svc.ConfirmAllocation(t.Context(), other, key, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("同键不同操作人应冲突: %v", err)
	}
	denied := p
	denied.Permissions = map[string]bool{}
	if _, err := svc.ConfirmAllocation(t.Context(), denied, key, in); !errors.Is(err, ErrForbidden) {
		t.Fatalf("撤销权限后仍可回放: %v", err)
	}
	outOfScope := p
	outOfScope.DataScopes = nil
	if _, err := svc.ConfirmAllocation(t.Context(), outOfScope, key, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("撤销范围后仍可回放: %v", err)
	}
	foreign := p
	foreign.TenantID = newID()
	if _, err := svc.ConfirmAllocation(t.Context(), foreign, key, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户回放未隔离: %v", err)
	}
}

// TestMySQLAllocationConcurrentReplayReturnsOneIdentity 验证同键并发请求只扣账一次并均返回相同标识。
// 参数：t为测试上下文，两个并发正式命令共用原键与规范内容。
// 返回值：无；并发失败、重复核销或返回不同标识时失败。
func TestMySQLAllocationConcurrentReplayReturnsOneIdentity(t *testing.T) {
	svc, p, receivable, _, in := allocationReplayFixture(t)
	key := newID()
	type outcome struct {
		id  string
		err error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			id, err := svc.ConfirmAllocation(t.Context(), p, key, in)
			outcomes <- outcome{id, err}
		}()
	}
	close(start)
	first, second := <-outcomes, <-outcomes
	if first.err != nil || second.err != nil || first.id == "" || first.id != second.id {
		t.Fatalf("并发返回ID=%s/%s，错误=%v/%v", first.id, second.id, first.err, second.err)
	}
	var count int
	if err := svc.DB.QueryRow(`SELECT COUNT(*) FROM settlement_receipt_allocation WHERE tenant_id=?`, p.TenantID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("并发核销数=%d err=%v", count, err)
	}
	assertCreditFlowState(t, svc.DB, p.TenantID, receivable, "100.00", "0.00", "SETTLED")
}
