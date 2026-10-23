package dunning

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type collectorStub struct{ eligible bool }

func (stub collectorStub) ContainsCollector(context.Context, string) (bool, error) {
	return stub.eligible, nil
}

func TestDunningLifecycleOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_DUNNING_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_DUNNING_TEST_MYSQL_DSN is required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{
		`DROP TABLE IF EXISTS settlement_dunning_action`,
		`DROP TABLE IF EXISTS settlement_dunning_case`,
		`DROP TABLE IF EXISTS settlement_dunning_policy`,
		`DROP TABLE IF EXISTS settlement_local_notification`,
		`DROP TABLE IF EXISTS settlement_outbox_event`,
		`DROP TABLE IF EXISTS settlement_handover_freeze`,
		`DROP TABLE IF EXISTS settlement_receivable`,
		`DROP TABLE IF EXISTS settlement_contract_snapshot`,
		`CREATE TABLE settlement_contract_snapshot (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,customer_name_snapshot VARCHAR(255) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_receivable (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,receivable_no VARCHAR(64) NOT NULL,contract_snapshot_id CHAR(32) NOT NULL,due_date DATE NOT NULL,open_amount DECIMAL(18,2) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_dunning_policy (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,name VARCHAR(128) NOT NULL,version INT NOT NULL,aging_from_days INT NOT NULL,aging_to_days INT NOT NULL,action_type VARCHAR(32) NOT NULL,recipient_rule VARCHAR(128) NOT NULL,channel VARCHAR(32) NOT NULL,repeat_interval_days INT NOT NULL,priority VARCHAR(16) NOT NULL,enabled BOOLEAN NOT NULL,created_at DATETIME(3) NOT NULL,updated_at DATETIME(3) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_dunning_case (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,receivable_id CHAR(32) NOT NULL,dunning_policy_id CHAR(32) NOT NULL,policy_version INT NOT NULL,current_escalation_level INT NOT NULL DEFAULT 0,status VARCHAR(32) NOT NULL,next_action_at DATETIME(3) NOT NULL,last_action_at DATETIME(3) NULL,closed_reason VARCHAR(255) NOT NULL DEFAULT '',closed_at DATETIME(3) NULL,created_at DATETIME(3) NOT NULL,updated_at DATETIME(3) NOT NULL,UNIQUE KEY uq_case(tenant_id,receivable_id,dunning_policy_id,policy_version)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_dunning_action (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,dunning_case_id CHAR(32) NOT NULL,action_type VARCHAR(32) NOT NULL,recipient_user_id VARCHAR(128) NOT NULL,channel VARCHAR(32) NOT NULL,priority VARCHAR(16) NOT NULL,event_id CHAR(32) NOT NULL,status VARCHAR(32) NOT NULL,version INT NOT NULL DEFAULT 1,sent_at DATETIME(3) NULL,completed_at DATETIME(3) NULL,completed_by VARCHAR(128) NOT NULL DEFAULT '',result_detail VARCHAR(500) NOT NULL DEFAULT '',updated_at DATETIME(3) NOT NULL,created_at DATETIME(3) NOT NULL,UNIQUE KEY uq_event(tenant_id,event_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_local_notification (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,recipient_user_id VARCHAR(128) NOT NULL,event_id CHAR(32) NOT NULL,notification_scope VARCHAR(32) NOT NULL,priority VARCHAR(16) NOT NULL,title VARCHAR(255) NOT NULL,content TEXT NOT NULL,target_url VARCHAR(500) NOT NULL,reference_type VARCHAR(64) NOT NULL,reference_id VARCHAR(128) NOT NULL,created_at DATETIME(3) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_outbox_event (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,event_id CHAR(32) NOT NULL,destination VARCHAR(64) NOT NULL,event_type VARCHAR(128) NOT NULL,aggregate_type VARCHAR(64) NOT NULL,aggregate_id VARCHAR(128) NOT NULL,payload_json JSON NOT NULL,status VARCHAR(32) NOT NULL,available_at DATETIME(3) NOT NULL,created_at DATETIME(3) NOT NULL,updated_at DATETIME(3) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_handover_freeze (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,batch_id CHAR(32) NOT NULL,request_id CHAR(32) NOT NULL,user_id VARCHAR(128) NOT NULL,status VARCHAR(16) NOT NULL,frozen_at DATETIME(3) NOT NULL,released_at DATETIME(3) NULL,outcome VARCHAR(16) NULL,created_at DATETIME(3) NOT NULL,updated_at DATETIME(3) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tenant, receivable, policy, snapshot := "tenant-dunning-life", "recv-dunning-life", "policy-dunning-life", "snap-dunning-life"
	if _, err = db.Exec(`INSERT INTO settlement_contract_snapshot(id,tenant_id,customer_name_snapshot) VALUES(?,?,?)`, snapshot, tenant, "测试客户"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,due_date,open_amount) VALUES(?,?,?,?,DATE_SUB(CURDATE(),INTERVAL 5 DAY),100.00)`, receivable, tenant, "YS-DUNNING", snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO settlement_dunning_policy(id,tenant_id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled,created_at,updated_at) VALUES(?,?,?,1,1,30,'REMIND','USER:collector-1','LOCAL',7,'NORMAL',TRUE,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, policy, tenant, "测试策略"); err != nil {
		t.Fatal(err)
	}
	scanner := &Scanner{DB: db, BatchSize: 50, Directory: collectorStub{eligible: true}}
	// 冻结接收者不得获得初始责任：冻结有效期间
	// 不能创建案件、动作或通知。
	if _, err = db.Exec(`INSERT INTO settlement_handover_freeze(id,tenant_id,batch_id,request_id,user_id,status,frozen_at,created_at,updated_at) VALUES('freeze-initial',?, 'batch-initial','request-initial','collector-1','FROZEN',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, tenant); err != nil {
		t.Fatal(err)
	}
	if err = scanner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, db, tenant, 0, 0, 0, 0)
	if _, err = db.Exec(`DELETE FROM settlement_handover_freeze WHERE tenant_id=?`, tenant); err != nil {
		t.Fatal(err)
	}
	if err = scanner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, db, tenant, 1, 1, 1, 1)
	var next time.Time
	if err = db.QueryRow(`SELECT next_action_at FROM settlement_dunning_case WHERE tenant_id=?`, tenant).Scan(&next); err != nil || next.Before(time.Now().UTC().Add(time.Hour)) {
		t.Fatalf("next_action_at=%v err=%v", next, err)
	}
	if err = scanner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, db, tenant, 1, 1, 1, 1)
	if _, err = db.Exec(`UPDATE settlement_dunning_case SET next_action_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 MINUTE) WHERE tenant_id=?`, tenant); err != nil {
		t.Fatal(err)
	}
	if err = scanner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, db, tenant, 1, 2, 2, 2)
	var level int
	if err = db.QueryRow(`SELECT current_escalation_level FROM settlement_dunning_case WHERE tenant_id=?`, tenant).Scan(&level); err != nil || level != 2 {
		t.Fatalf("level=%d err=%v", level, err)
	}
	if _, err = db.Exec(`UPDATE settlement_receivable SET open_amount=0 WHERE tenant_id=?`, tenant); err != nil {
		t.Fatal(err)
	}
	if err = scanner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	var cancelled int
	if err = db.QueryRow(`SELECT status FROM settlement_dunning_case WHERE tenant_id=?`, tenant).Scan(&status); err != nil || status != "CLOSED" {
		t.Fatalf("closed status=%s err=%v", status, err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=? AND status='CANCELLED'`, tenant).Scan(&cancelled); err != nil || cancelled != 2 {
		t.Fatalf("cancelled=%d err=%v", cancelled, err)
	}
	if _, err = db.Exec(`UPDATE settlement_receivable SET open_amount=100.00 WHERE tenant_id=?`, tenant); err != nil {
		t.Fatal(err)
	}
	if err = scanner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT status FROM settlement_dunning_case WHERE tenant_id=?`, tenant).Scan(&status); err != nil || status != "ACTIVE" {
		t.Fatalf("reopened status=%s err=%v", status, err)
	}
	var actions int
	if err = db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=?`, tenant).Scan(&actions); err != nil || actions != 3 {
		t.Fatalf("actions after reopen=%d err=%v", actions, err)
	}
	if _, err = db.Exec(`INSERT INTO settlement_handover_freeze(id,tenant_id,batch_id,request_id,user_id,status,frozen_at,created_at,updated_at) VALUES('freeze-1',?, 'batch-1','request-1','collector-1','FROZEN',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE settlement_dunning_case SET next_action_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 MINUTE) WHERE tenant_id=?`, tenant); err != nil {
		t.Fatal(err)
	}
	if err = scanner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=?`, tenant).Scan(&actions); err != nil || actions != 3 {
		t.Fatalf("frozen action count=%d err=%v", actions, err)
	}
}

func assertCounts(t *testing.T, db *sql.DB, tenant string, cases, actions, pending, notifications int) {
	t.Helper()
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM settlement_dunning_case WHERE tenant_id=?`:                        cases,
		`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=?`:                      actions,
		`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=? AND status='PENDING'`: pending,
		`SELECT COUNT(*) FROM settlement_local_notification WHERE tenant_id=?`:                  notifications,
	} {
		var got int
		if err := db.QueryRow(query, tenant).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d err=%v, want %d", strings.TrimSpace(query), got, err, want)
		}
	}
}
