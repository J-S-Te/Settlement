package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeCreditPublisher struct {
	fail    bool
	tenants []string
	events  []string
}

func (p *fakeCreditPublisher) Publish(_ context.Context, tenant, eventID string, _ []byte) error {
	p.tenants = append(p.tenants, tenant)
	p.events = append(p.events, eventID)
	if p.fail {
		return errors.New("temporary CRM outage")
	}
	return nil
}

func TestCreditSyncWorkerClaimsRetriesAndDelivers(t *testing.T) {
	db, suffix := newCreditSyncMySQLTestDB(t)
	otherDB, otherSuffix := newCreditSyncMySQLTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 第二个自有命名空间代表另一次运行。其符合条件的任务必须
	// 保持不变，即使生产工作器跨租户认领。
	otherID := "SYNC-" + otherSuffix
	if _, err := otherDB.ExecContext(ctx, `INSERT INTO settlement_credit_sync(id,tenant_id,event_id,allocation_id,receivable_id,status,payload_json,created_at,updated_at) VALUES(?,?,?,?,?,'PENDING','{}',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		otherID, "credit-worker-"+otherSuffix, "E2E-"+otherSuffix, "ALLOC-"+otherSuffix, "RECV-"+otherSuffix); err != nil {
		t.Fatal(err)
	}
	tenant := "credit-worker-" + suffix
	eventID := "E2E-" + suffix
	allocationID := "ALLOC-" + suffix
	if _, err := db.ExecContext(ctx, `INSERT INTO settlement_credit_sync(id,tenant_id,event_id,event_type,original_event_id,allocation_id,receivable_id,status,payload_json,created_at,updated_at) VALUES(?,?,?,?,'',?,?, 'PENDING',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		"SYNC-"+suffix, tenant, eventID, creditEventCompleted, allocationID, "RECV-"+suffix, []byte(`{"event_id":"`+eventID+`"}`)); err != nil {
		t.Fatal(err)
	}
	publisher := &fakeCreditPublisher{fail: true}
	worker := &CreditSyncWorker{DB: db, Publisher: publisher, BatchSize: 10, Lease: time.Minute}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT status,attempt_count FROM settlement_credit_sync WHERE tenant_id=? AND event_id=?`, tenant, eventID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "RETRY_WAIT" || attempts != 1 || len(publisher.events) != 1 || publisher.tenants[0] != tenant {
		t.Fatalf("after failure status=%s attempts=%d events=%v tenants=%v", status, attempts, publisher.events, publisher.tenants)
	}
	publisher.fail = false
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status,attempt_count FROM settlement_credit_sync WHERE tenant_id=? AND event_id=?`, tenant, eventID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "DELIVERED" || attempts != 2 {
		t.Fatalf("after retry status=%s attempts=%d", status, attempts)
	}
	if err := otherDB.QueryRowContext(ctx, `SELECT status,attempt_count FROM settlement_credit_sync WHERE id=?`, otherID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING" || attempts != 0 {
		t.Fatalf("another run's event was claimed: status=%s attempts=%d", status, attempts)
	}
}
