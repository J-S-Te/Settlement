package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

type invoiceAvailabilityResult struct {
	amount string
	err    error
}

// invoiceReservationRaceFixture 建立原额100且尚无预留的独占应收。
// 参数：t 为测试上下文，仅显式隔离DSN下创建随机schema。
// 返回值：服务、合同快照和应收标识。
func invoiceReservationRaceFixture(t *testing.T) (*Service, string, string) {
	t.Helper()
	db, _ := invoiceArchiveMySQL(t)
	snapshot, receivable := newID(), newID()
	_, err := db.Exec(`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,'tenant-reservation','contract_management','event','contract','HT-1',1,'customer','customer','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,due_date,original_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,'tenant-reservation',?,?,CURDATE(),100,100,'CNY','CONFIRMED','UNPAID','UNINVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, receivable, "R-"+receivable, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{DB: db}, snapshot, receivable
}

// insertInvoiceReservationRaceRow 模拟先持应收锁的事务A真实落地70元预留。
// 参数：ctx 控制SQL，tx 为事务A，snapshot 为合同，receivable 为被预留应收。
// 返回值：存储错误。
func insertInvoiceReservationRaceRow(ctx context.Context, tx *sql.Tx, snapshot, receivable string) error {
	id := newID()
	_, err := tx.ExecContext(ctx, `INSERT INTO settlement_invoice_request(id,tenant_id,request_no,contract_snapshot_id,buyer_profile_snapshot,invoice_type,amount_excl_tax,tax_amount,amount_incl_tax,status,idempotency_key,submitted_by,created_at,updated_at) VALUES(?,'tenant-reservation',?,?,'{}','VAT_NORMAL',70,0,70,'SUBMITTED',?,'actor',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, "IR-"+id, snapshot, id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_request_allocation(id,tenant_id,invoice_request_id,receivable_id,reserved_amount,status) VALUES(?,'tenant-reservation',?,?,70,'RESERVED')`, newID(), id, receivable)
	return err
}

// waitInvoiceReservationLock 核验指定schema中应收行出现真实等待，避免用时间猜测并发窗口。
// 参数：t 为测试上下文，ctx 控制查询，db 为独占schema，minimum 为等待连接数。
// 返回值：无；无法在期限内观察等待则失败。
func waitInvoiceReservationLock(t *testing.T, ctx context.Context, db *sql.DB, minimum int) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var count int
		err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT w.REQUESTING_THREAD_ID) FROM performance_schema.data_lock_waits w JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID=w.REQUESTING_ENGINE_LOCK_ID AND l.ENGINE=w.ENGINE WHERE l.OBJECT_SCHEMA=DATABASE() AND l.OBJECT_NAME='settlement_receivable'`).Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
		if count >= minimum {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-deadline.C:
			t.Fatalf("应收锁等待不足：%d，需要%d", count, minimum)
		case <-ticker.C:
		}
	}
}

// TestMySQLInvoiceReservationAvailabilityUsesCurrentRows 验证旧RR快照不能隐藏先提交的70元预留。
// 参数：t 为测试上下文；仅本次显式隔离MySQL执行。
// 返回值：无；等待锁后余额必须为30.00。
func TestMySQLInvoiceReservationAvailabilityUsesCurrentRows(t *testing.T) {
	s, snapshot, receivable := invoiceReservationRaceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	txB, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback()
	var id string
	if err = txB.QueryRowContext(ctx, `SELECT id FROM settlement_contract_snapshot WHERE id=?`, snapshot).Scan(&id); err != nil {
		t.Fatal(err)
	}
	txA, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback()
	if err = txA.QueryRowContext(ctx, `SELECT id FROM settlement_receivable WHERE id=? FOR UPDATE`, receivable).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err = insertInvoiceReservationRaceRow(ctx, txA, snapshot, receivable); err != nil {
		t.Fatal(err)
	}
	done := make(chan invoiceAvailabilityResult, 1)
	go func() {
		available, err := invoiceAvailableForUpdate(
			ctx, txB, "tenant-reservation", snapshot, receivable,
		)
		done <- invoiceAvailabilityResult{amount: available, err: err}
	}()
	waitInvoiceReservationLock(t, ctx, s.DB, 1)
	if err = txA.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.amount != "30.00" {
			t.Fatalf("旧快照漏计已提交预留，余额=%q err=%v", result.amount, result.err)
		}
	case <-ctx.Done():
		t.Fatal("释放应收锁后余额读取未结束")
	}
}

// TestMySQLConcurrentInvoiceRequestsCannotOverReserve 验证同100元应收同时申请70元只可成功一笔。
// 参数：t 为测试上下文；使用两次真实业务调用和独占schema。
// 返回值：无；申请必须有一成功一冲突，最终预留为70.00。
func TestMySQLConcurrentInvoiceRequestsCannotOverReserve(t *testing.T) {
	s, snapshot, receivable := invoiceReservationRaceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	gate, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	var id string
	if err = gate.QueryRowContext(ctx, `SELECT id FROM settlement_receivable WHERE id=? FOR UPDATE`, receivable).Scan(&id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for _, key := range []string{"reservation-a", "reservation-b"} {
		go func(key string) {
			p := Principal{TenantID: "tenant-reservation", UserID: key, Permissions: map[string]bool{"settlement.invoice.request": true}}
			in := InvoiceRequestInput{ContractSnapshotID: snapshot, InvoiceType: "VAT_NORMAL", Items: []InvoiceItemInput{{ItemName: "service", Quantity: "1", UnitPriceExclTax: "70", AmountExclTax: "70", TaxRate: "0", TaxAmount: "0", AmountInclTax: "70"}}, Allocations: []InvoiceAllocationInput{{ReceivableID: receivable, ReservedAmount: "70"}}}
			_, err := s.CreateInvoiceRequest(ctx, p, key, in)
			done <- err
		}(key)
	}
	waitInvoiceReservationLock(t, ctx, s.DB, 2)
	if err = gate.Commit(); err != nil {
		t.Fatal(err)
	}
	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err == nil {
				successes++
			} else if errors.Is(err, ErrConflict) {
				conflicts++
			} else {
				t.Errorf("开票并发出现非预期错误：%v", err)
			}
		case <-ctx.Done():
			t.Fatal("并发开票申请未在期限内结束")
		}
	}
	var reserved string
	if err = s.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_amount-invoiced_amount-released_amount),0) FROM settlement_invoice_request_allocation WHERE tenant_id='tenant-reservation' AND receivable_id=? AND status='RESERVED'`, receivable).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 || reserved != "70.00" {
		t.Fatalf("并发超预留：成功%d 冲突%d 实际预留%s", successes, conflicts, reserved)
	}
}
