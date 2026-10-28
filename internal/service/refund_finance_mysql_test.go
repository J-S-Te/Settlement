package service

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// refundFinanceMySQL 限制退款验收仅使用显式指定的回环隔离库。
// 参数：t 测试上下文，环境变量由主验证器提供。
// 返回值：应用全部真实迁移的独占随机schema连接。
func refundFinanceMySQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_TEST_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_TEST_DSN absent; isolated refund SQL acceptance not executed")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || cfg.Net != "tcp" || !(strings.HasPrefix(cfg.Addr, "127.0.0.1:") || strings.HasPrefix(cfg.Addr, "localhost:")) {
		t.Fatal("refund acceptance requires explicit loopback isolated DSN; DSN withheld")
	}
	db, _ := invoiceArchiveMySQL(t)
	return db
}

// seedRefundReceipt 建立金额固定且日期确定的隔离回款事实。
// 参数：t 测试上下文，db 独占测试库，tenant 测试租户。
// 返回值：新回款ID。
func seedRefundReceipt(t *testing.T, db *sql.DB, tenant string) string {
	t.Helper()
	id := newID()
	_, err := db.Exec(`INSERT INTO settlement_receipt
 (id,tenant_id,receipt_no,customer_id,customer_name_snapshot,amount,unallocated_amount,
 currency,receipt_date,payment_method,bank_transaction_reference,source_type,status,
 idempotency_key,created_at,updated_at)
 VALUES(?,?,?,'customer','隔离客户',100,100,'CNY','2020-01-01','BANK',?,'MANUAL',
 'CONFIRMED',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, id, id, id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestMySQLRefundConfirmationIsAtomicAndKeepsHeldBalance 验证自然审批、并发确认及审计金额不变量。
// 参数：t 测试上下文；仅在主验证器提供显式隔离DSN时运行。
// 返回值：无；任何重复确认、事实重写或余额变化时失败。
func TestMySQLRefundConfirmationIsAtomicAndKeepsHeldBalance(t *testing.T) {
	db := refundFinanceMySQL(t)
	p := refundTestPrincipal()
	receipt := seedRefundReceipt(t, db, p.TenantID)
	svc := &Service{DB: db}
	id, err := svc.CreateReceiptDifference(t.Context(), p, ReceiptDifferenceInput{Key: newID(), ReceiptID: receipt, Kind: "UNKNOWN", Amount: "20", Reason: "退款暂挂"})
	if err != nil {
		t.Fatal(err)
	}
	in := refundTestInput()
	in.Version = 1
	if err = svc.ConfirmReceiptRefund(t.Context(), p, id, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("held: %v", err)
	}
	if err = svc.ResolveReceiptDifference(t.Context(), p, id, DifferenceResolutionInput{Version: 1, Resolution: "REFUND_PENDING", Reason: "退回客户"}); err != nil {
		t.Fatal(err)
	}
	in.Version = 2
	if err = svc.ConfirmReceiptRefund(t.Context(), p, id, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("unapproved: %v", err)
	}
	manager := Principal{TenantID: p.TenantID, UserID: "manager", Permissions: map[string]bool{"settlement.difference.approve": true}}
	if err = svc.ApproveReceiptDifference(t.Context(), manager, id, 2); err != nil {
		t.Fatal(err)
	}
	if err = svc.ConfirmReceiptRefund(t.Context(), p, id, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
	in.Version = 3
	bad := in
	bad.CompletedAt = "2019-12-31T23:59:59Z"
	if err = svc.ConfirmReceiptRefund(t.Context(), p, id, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("before receipt: %v", err)
	}
	foreign := p
	foreign.TenantID = "foreign"
	if err = svc.ConfirmReceiptRefund(t.Context(), foreign, id, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross tenant: %v", err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; results <- svc.ConfirmReceiptRefund(t.Context(), p, id, in) }()
	}
	close(start)
	success, conflict := 0, 0
	for i := 0; i < 2; i++ {
		err = <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	in.Version = 4
	in.Reference = "rewrite"
	if err = svc.ConfirmReceiptRefund(t.Context(), p, id, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("rewrite: %v", err)
	}
	assertRefundFacts(t, db, id, receipt, p)
	items, err := svc.ListFinanceRecords(t.Context(), p, false, FinanceRecordQuery{Limit: 50, Status: "REFUND_CONFIRMED"})
	if err != nil || len(items) != 1 || items[0]["refund_reference"] != "银行退款-1" || items[0]["refund_completed_at"] != "2020-01-02T02:00:00.000000Z" {
		t.Fatalf("list facts=%v err=%v", items, err)
	}
}

// assertRefundFacts 核对确认后的事实、单次审计和回款余额版本。
// 参数：t 测试上下文，db 隔离库，id 差异，receipt 回款，p 执行主体。
// 返回值：无；任一持久化不变量不满足时失败。
func assertRefundFacts(t *testing.T, db *sql.DB, id, receipt string, p Principal) {
	t.Helper()
	var status, reference, actor, reason, completed string
	var version int
	err := db.QueryRow(`SELECT status,refund_reference,refund_confirmed_by,refund_reason,
 DATE_FORMAT(refund_completed_at,'%Y-%m-%d %H:%i:%s'),version
 FROM settlement_receipt_difference WHERE id=?`, id).Scan(&status, &reference, &actor, &reason, &completed, &version)
	if err != nil || status != "REFUND_CONFIRMED" || reference != "银行退款-1" || actor != p.UserID || reason != "线下退款已完成" || completed != "2020-01-02 02:00:00" || version != 4 {
		t.Fatalf("facts=%s %s %s %s %s %d err=%v", status, reference, actor, reason, completed, version, err)
	}
	var total, balance string
	if err = db.QueryRow(`SELECT amount,unallocated_amount,version FROM settlement_receipt WHERE id=?`, receipt).Scan(&total, &balance, &version); err != nil || total != "100.00" || balance != "80.00" || version != 2 {
		t.Fatalf("money=%s/%s version=%d err=%v", total, balance, version, err)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM settlement_audit_event WHERE resource_id=? AND action='SETTLEMENT_REFUND_CONFIRMED'`, `SELECT COUNT(*) FROM settlement_outbox_event WHERE aggregate_id=? AND event_type='SETTLEMENT_REFUND_CONFIRMED'`} {
		var count int
		if err = db.QueryRow(q, id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("refund event count=%d err=%v", count, err)
		}
	}
}

// TestMySQLFinancePagesReachOldPendingRecords 验证真实分页越过200、筛选和稳定排序。
// 参数：t 测试上下文；只在显式隔离DSN配置时运行。
// 返回值：无；旧待处理对象不可达、串租户或筛选丢失时失败。
func TestMySQLFinancePagesReachOldPendingRecords(t *testing.T) {
	db := refundFinanceMySQL(t)
	p := refundTestPrincipal()
	p.Permissions["settlement.voucher.manage"] = true
	receipt := seedRefundReceipt(t, db, p.TenantID)
	seedFinancePageRecords(t, db, p.TenantID, receipt)
	svc := &Service{DB: db}
	for _, vouchers := range []bool{false, true} {
		status := "REFUND_PENDING"
		if vouchers {
			status = "FAILED"
		}
		items, err := svc.ListFinanceRecords(t.Context(), p, vouchers)
		if err != nil || len(items) != 100 {
			t.Fatalf("default size=%d err=%v", len(items), err)
		}
		items, err = svc.ListFinanceRecords(t.Context(), p, vouchers, FinanceRecordQuery{Limit: 50, Offset: 200})
		if err != nil || len(items) != 21 || items[20]["id"] != "00000000000000000000000000000220" {
			t.Fatalf("old page=%v err=%v", items, err)
		}
		items, err = svc.ListFinanceRecords(t.Context(), p, vouchers, FinanceRecordQuery{Limit: 50, Offset: 200, Status: status})
		if err != nil || len(items) != 20 || items[19]["id"] != "00000000000000000000000000000220" {
			t.Fatalf("filtered old page=%v err=%v", items, err)
		}
		items, err = svc.ListFinanceRecords(t.Context(), p, vouchers, FinanceRecordQuery{Limit: 50, Keyword: "ancient-target"})
		if err != nil || len(items) != 1 || items[0]["id"] != "00000000000000000000000000000220" {
			t.Fatalf("keyword=%v err=%v", items, err)
		}
		items, err = svc.ListFinanceRecords(t.Context(), p, vouchers, FinanceRecordQuery{Limit: 50, Keyword: "%"})
		if err != nil || len(items) != 0 {
			t.Fatalf("literal keyword=%v err=%v", items, err)
		}
		items, err = svc.ListFinanceRecords(t.Context(), p, vouchers, FinanceRecordQuery{Limit: 50, Offset: 250})
		if err != nil || len(items) != 0 {
			t.Fatalf("empty page=%v err=%v", items, err)
		}
	}
}

// seedFinancePageRecords 写入同时间戳的多页财务事实和不可见异租户记录。
// 参数：t 测试上下文，db 隔离库，tenant 租户，receipt 关联回款。
// 返回值：无。
func seedFinancePageRecords(t *testing.T, db *sql.DB, tenant, receipt string) {
	t.Helper()
	for i := 0; i < 222; i++ {
		id := fmt.Sprintf("%032d", i)
		rowTenant := tenant
		differenceStatus, voucherStatus := "REFUND_PENDING", "FAILED"
		if i == 0 {
			differenceStatus, voucherStatus = "HELD", "SENT"
		}
		if i == 221 {
			rowTenant = "foreign"
		}
		keyword := "normal"
		if i >= 220 {
			keyword = "ancient-target"
		}
		_, err := db.Exec(`INSERT INTO settlement_receipt_difference
 (id,tenant_id,receipt_id,kind,amount,reason,status,requested_by,created_at)
 VALUES(?,?,?,'UNKNOWN',1,?,?,'accountant','2020-01-01 00:00:00')`, id, rowTenant, receipt, keyword, differenceStatus)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`INSERT INTO settlement_accounting_voucher
 (id,tenant_id,writeoff_id,payload,status,external_no,created_at)
 VALUES(?,?,?,'{}',?,?,'2020-01-01 00:00:00')`, id, rowTenant, id, voucherStatus, keyword)
		if err != nil {
			t.Fatal(err)
		}
	}
}
