package service

import (
	"database/sql"
	"errors"
	"testing"
)

// seedDifferenceReceivable 建立真实核销所需的100元合同、已确认计划和应收。
// 参数：t为测试上下文，db为独占隔离库，tenant为合成租户。
// 返回值：可供正式ConfirmAllocation消费的应收标识。
func seedDifferenceReceivable(t *testing.T, db *sql.DB, tenant string) string {
	t.Helper()
	snapshot, plan, receivable := newID(), newID(), newID()
	steps := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO settlement_contract_snapshot
		 (id,tenant_id,source_application,source_event_id,source_contract_id,
		 source_contract_no,source_contract_version,customer_id,customer_name_snapshot,
		 currency,contract_amount,financial_status,effective_at,created_at,updated_at)
		 VALUES(?,?,'contract_management',?,?,?,1,'customer','隔离客户','CNY',100,
		 'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
			[]any{snapshot, tenant, newID(), newID(), newID()}},
		{`INSERT INTO settlement_receivable_plan
		 (id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,
		 generation_source,confirmation_status,created_at,updated_at)
		 VALUES(?,?,?,1,'2026-01-01',100,'CONTRACT_EVENT','CONFIRMED',
		 UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{plan, tenant, snapshot}},
		{`INSERT INTO settlement_receivable
		 (id,tenant_id,receivable_no,contract_snapshot_id,receivable_plan_id,due_date,
		 original_amount,open_amount,currency,recognition_status,collection_status,
		 invoice_status,created_at,updated_at)
		 VALUES(?,?,?,?,?,'2026-01-01',100,100,'CNY','CONFIRMED','UNPAID',
		 'UNINVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
			[]any{receivable, tenant, newID(), snapshot, plan}},
	}
	for _, step := range steps {
		if _, err := db.ExecContext(t.Context(), step.query, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	return receivable
}

// createDifferenceReceipt 通过正式回款服务记录100元合成银行回款。
// 参数：t为测试上下文，svc为独占库服务，p为回款主体。
// 返回值：初始状态为AVAILABLE的回款标识。
func createDifferenceReceipt(t *testing.T, svc *Service, p Principal) string {
	t.Helper()
	id, err := svc.CreateReceipt(t.Context(), p, newID(), ReceiptInput{
		CustomerID: "customer", CustomerName: "隔离客户", Amount: "100.00",
		Currency: "CNY", ReceiptDate: "2026-01-01", PaymentMethod: "TRANSFER",
		BankReference: newID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDifferenceReceipt(t, svc.DB, p.TenantID, id, "100.00", "AVAILABLE", 1)
	return id
}

// assertDifferenceReceipt 核验差异事务提交后的真实余额、状态和版本。
// 参数：t为测试上下文，db为隔离库，tenant/id为回款归属，其余为独立预期。
// 返回值：无；任何余额、状态或版本不符时失败。
func assertDifferenceReceipt(t *testing.T, db *sql.DB, tenant, id, balance, status string, version int) {
	t.Helper()
	var gotBalance, gotStatus string
	var gotVersion int
	err := db.QueryRowContext(t.Context(), `SELECT unallocated_amount,status,version
	 FROM settlement_receipt WHERE tenant_id=? AND id=?`, tenant, id).
		Scan(&gotBalance, &gotStatus, &gotVersion)
	if err != nil || gotBalance != balance || gotStatus != status || gotVersion != version {
		t.Fatalf("回款余额=%s 状态=%s 版本=%d，期望%s/%s/%d，err=%v",
			gotBalance, gotStatus, gotVersion, balance, status, version, err)
	}
}

// TestMySQLReceiptDifferenceReleaseAllowsFurtherAllocation 复现暂挂释放后余额无法继续核销。
// 参数：t为测试上下文；只使用SETTLEMENT_TEST_DSN指定的独占隔离实例。
// 返回值：无；两类非退款处置均须恢复可核销状态并完成剩余20元核销。
func TestMySQLReceiptDifferenceReleaseAllowsFurtherAllocation(t *testing.T) {
	db := refundFinanceMySQL(t)
	for _, resolution := range []string{"CARRY_FORWARD", "MATCHED"} {
		for _, kind := range []string{"UNKNOWN", "OVERPAYMENT"} {
			t.Run(kind+"/"+resolution, func(t *testing.T) {
				svc := &Service{DB: db}
				p := roleFlowPrincipal("settlement_accountant", "requester", newID())
				receivable := seedDifferenceReceivable(t, db, p.TenantID)
				receipt := createDifferenceReceipt(t, svc, p)
				difference := holdDifferenceForReview(t, svc, p, receipt, kind, resolution)
				if _, err := svc.ConfirmAllocation(t.Context(), p, newID(), AllocationInput{
					ReceiptID: receipt, ReceivableID: receivable, Amount: "80", MatchMode: "MANUAL",
				}); err != nil {
					t.Fatal(err)
				}
				assertDifferenceReceipt(t, db, p.TenantID, receipt, "0.00", "FULLY_ALLOCATED", 3)
				approver := roleFlowPrincipal("settlement_finance_manager", "approver", p.TenantID)
				if err := svc.ApproveReceiptDifference(t.Context(), approver, difference, 2); err != nil {
					t.Fatal(err)
				}
				assertDifferenceReceipt(t, db, p.TenantID, receipt, "20.00", "PARTIALLY_ALLOCATED", 4)
				if _, err := svc.ConfirmAllocation(t.Context(), p, newID(), AllocationInput{
					ReceiptID: receipt, ReceivableID: receivable, Amount: "20", MatchMode: "MANUAL",
				}); err != nil {
					t.Fatalf("释放后的20元无法继续核销: %v", err)
				}
				assertDifferenceReceipt(t, db, p.TenantID, receipt, "0.00", "FULLY_ALLOCATED", 5)
				assertDifferenceAllocation(t, db, p.TenantID, receivable)
			})
		}
	}
}

// holdDifferenceForReview 通过正式暂挂和处置服务创建20元待独立审批差异。
// 参数：t为测试上下文，svc为服务，p为申请人，receipt为回款，kind/resolution为规则组合。
// 返回值：已进入REVIEW_PENDING且版本为2的差异标识。
func holdDifferenceForReview(t *testing.T, svc *Service, p Principal, receipt, kind, resolution string) string {
	t.Helper()
	id, err := svc.CreateReceiptDifference(t.Context(), p, ReceiptDifferenceInput{
		Key: newID(), ReceiptID: receipt, Kind: kind, Amount: "20", Reason: "隔离差异暂挂",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := roleFlowPrincipal("settlement_finance_manager", "resolver", p.TenantID)
	if err = svc.ResolveReceiptDifference(t.Context(), resolver, id, DifferenceResolutionInput{
		Version: 1, Resolution: resolution, Reason: "隔离差异处置",
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// assertDifferenceAllocation 验证两次正式核销及应收结清持久化事实。
// 参数：t为测试上下文，db为独占库，tenant/receivable为合成应收归属。
// 返回值：无；金额、数量或应收结清事实不符时失败。
func assertDifferenceAllocation(t *testing.T, db *sql.DB, tenant, receivable string) {
	t.Helper()
	var count int
	var allocated, open, state string
	err := db.QueryRowContext(t.Context(), `SELECT COUNT(*),SUM(allocated_amount)
	 FROM settlement_receipt_allocation WHERE tenant_id=? AND receivable_id=?
	 AND status='CONFIRMED'`, tenant, receivable).Scan(&count, &allocated)
	if err != nil || count != 2 || allocated != "100.00" {
		t.Fatalf("核销数量=%d 合计=%s err=%v", count, allocated, err)
	}
	err = db.QueryRowContext(t.Context(), `SELECT open_amount,collection_status
	 FROM settlement_receivable WHERE tenant_id=? AND id=?`, tenant, receivable).Scan(&open, &state)
	if err != nil || open != "0.00" || state != "SETTLED" {
		t.Fatalf("应收余额=%s 状态=%s err=%v", open, state, err)
	}
}

// TestMySQLReceiptDifferenceApprovalKeepsBoundaries 验证独立审批和非释放处置的余额规则。
// 参数：t为测试上下文；真实数据库只取显式回环隔离DSN。
// 返回值：无；跨租户、重复审批、版本、角色或退款/短款边界失效时失败。
func TestMySQLReceiptDifferenceApprovalKeepsBoundaries(t *testing.T) {
	db := refundFinanceMySQL(t)
	for _, c := range []struct {
		kind, resolution, balance, state string
		version                          int
	}{
		{"UNKNOWN", "CARRY_FORWARD", "100.00", "APPROVED", 3},
		{"UNKNOWN", "REFUND_PENDING", "80.00", "REFUND_PENDING", 2},
		{"SHORTPAYMENT", "MATCHED", "100.00", "APPROVED", 1},
	} {
		t.Run(c.kind+"/"+c.resolution, func(t *testing.T) {
			svc := &Service{DB: db}
			p := roleFlowPrincipal("settlement_accountant", "requester", newID())
			receipt := createDifferenceReceipt(t, svc, p)
			id := holdDifferenceForReview(t, svc, p, receipt, c.kind, c.resolution)
			approver := roleFlowPrincipal("settlement_finance_manager", "approver", p.TenantID)
			for _, user := range []string{"requester", "resolver"} {
				actor := approver
				actor.UserID = user
				if err := svc.ApproveReceiptDifference(t.Context(), actor, id, 2); !errors.Is(err, ErrForbidden) {
					t.Fatalf("%s不应独立审批: %v", user, err)
				}
			}
			foreign := approver
			foreign.TenantID = "foreign"
			if err := svc.ApproveReceiptDifference(t.Context(), foreign, id, 2); !errors.Is(err, ErrConflict) {
				t.Fatalf("跨租户审批: %v", err)
			}
			if err := svc.ApproveReceiptDifference(t.Context(), approver, id, 1); !errors.Is(err, ErrConflict) {
				t.Fatalf("旧版本审批: %v", err)
			}
			if err := svc.ApproveReceiptDifference(t.Context(), approver, id, 2); err != nil {
				t.Fatal(err)
			}
			if err := svc.ApproveReceiptDifference(t.Context(), approver, id, 2); !errors.Is(err, ErrConflict) {
				t.Fatalf("重复审批: %v", err)
			}
			assertDifferenceReceipt(t, db, p.TenantID, receipt, c.balance, "AVAILABLE", c.version)
			var state string
			var version int
			if err := db.QueryRowContext(t.Context(), `SELECT status,version
			 FROM settlement_receipt_difference WHERE tenant_id=? AND id=?`, p.TenantID, id).
				Scan(&state, &version); err != nil || state != c.state || version != 3 {
				t.Fatalf("差异状态=%s 版本=%d err=%v", state, version, err)
			}
		})
	}
}

// TestMySQLReceiptDifferenceReleaseRejectsInactiveReceipt 验证异常回款不能借差异审批恢复可核销状态。
// 参数：t为测试上下文；CANCELLED是隔离库中显式构造的未来终态边界。
// 返回值：无；审批须冲突且回款、差异和审批审计均不改变。
func TestMySQLReceiptDifferenceReleaseRejectsInactiveReceipt(t *testing.T) {
	db := refundFinanceMySQL(t)
	svc := &Service{DB: db}
	p := roleFlowPrincipal("settlement_accountant", "requester", newID())
	receipt := createDifferenceReceipt(t, svc, p)
	id := holdDifferenceForReview(t, svc, p, receipt, "UNKNOWN", "CARRY_FORWARD")
	if _, err := db.ExecContext(t.Context(), `UPDATE settlement_receipt SET status='CANCELLED'
	 WHERE tenant_id=? AND id=?`, p.TenantID, receipt); err != nil {
		t.Fatal(err)
	}
	approver := roleFlowPrincipal("settlement_finance_manager", "approver", p.TenantID)
	if err := svc.ApproveReceiptDifference(t.Context(), approver, id, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("异常回款不应审批释放: %v", err)
	}
	assertDifferenceReceipt(t, db, p.TenantID, receipt, "80.00", "CANCELLED", 2)
	var state, approvedBy string
	var version, audits int
	err := db.QueryRowContext(t.Context(), `SELECT status,approved_by,version
	 FROM settlement_receipt_difference WHERE tenant_id=? AND id=?`, p.TenantID, id).
		Scan(&state, &approvedBy, &version)
	if err != nil || state != "REVIEW_PENDING" || approvedBy != "" || version != 2 {
		t.Fatalf("失败审批未回滚: 状态=%s 审批人=%s 版本=%d err=%v", state, approvedBy, version, err)
	}
	err = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM settlement_outbox_event
	 WHERE tenant_id=? AND aggregate_id=? AND event_type='SETTLEMENT_DIFFERENCE_APPROVED'`,
		p.TenantID, id).Scan(&audits)
	if err != nil || audits != 0 {
		t.Fatalf("失败审批产生成功审计: count=%d err=%v", audits, err)
	}
}
