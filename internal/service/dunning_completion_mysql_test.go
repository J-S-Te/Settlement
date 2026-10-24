package service

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

func TestCompleteDunningActionOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_DUNNING_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_DUNNING_TEST_MYSQL_DSN is required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS settlement_dunning_action`,
		`DROP TABLE IF EXISTS settlement_outbox_event`,
		`DROP TABLE IF EXISTS settlement_audit_event`,
		`CREATE TABLE settlement_dunning_action (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,dunning_case_id CHAR(32) NOT NULL,action_type VARCHAR(32) NOT NULL,recipient_user_id VARCHAR(128) NOT NULL,channel VARCHAR(32) NOT NULL,priority VARCHAR(16) NOT NULL,event_id CHAR(32) NOT NULL,status VARCHAR(32) NOT NULL,version INT NOT NULL DEFAULT 1,sent_at DATETIME(3) NULL,completed_at DATETIME(3) NULL,completed_by VARCHAR(128) NOT NULL DEFAULT '',result_detail VARCHAR(500) NOT NULL DEFAULT '',updated_at DATETIME(3) NOT NULL,created_at DATETIME(3) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_outbox_event (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,event_id CHAR(32) NOT NULL,destination VARCHAR(64) NOT NULL,event_type VARCHAR(128) NOT NULL,aggregate_type VARCHAR(64) NOT NULL,aggregate_id VARCHAR(128) NOT NULL,payload_json JSON NOT NULL,status VARCHAR(32) NOT NULL,available_at DATETIME(3) NOT NULL,created_at DATETIME(3) NOT NULL,updated_at DATETIME(3) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_audit_event (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,actor_id VARCHAR(128) NOT NULL,action VARCHAR(128) NOT NULL,resource_type VARCHAR(64) NOT NULL,resource_id VARCHAR(128) NOT NULL,result VARCHAR(16) NOT NULL,reason_code VARCHAR(64) NOT NULL,request_id VARCHAR(64) NOT NULL,correlation_id VARCHAR(64) NOT NULL,detail_json JSON NOT NULL,occurred_at DATETIME(3) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tenant := "tenant-dunning-complete"
	if _, err := db.Exec(`INSERT INTO settlement_dunning_action(id,tenant_id,dunning_case_id,action_type,recipient_user_id,channel,priority,event_id,status,version,sent_at,updated_at,created_at) VALUES('action-dunning-1',?, 'case-1','REMIND','collector-1','LOCAL','NORMAL','event-dunning-1','PENDING',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, tenant); err != nil {
		t.Fatal(err)
	}
	service := &Service{DB: db}
	collector := Principal{TenantID: tenant, UserID: "collector-1", Permissions: map[string]bool{}}
	if err := service.CompleteDunningAction(context.Background(), collector, "action-dunning-1", 1, "已电话联系客户"); err != nil {
		t.Fatal(err)
	}
	var status, completedBy, note string
	var version uint64
	if err := db.QueryRow(`SELECT status,completed_by,result_detail,version FROM settlement_dunning_action WHERE id='action-dunning-1'`).Scan(&status, &completedBy, &note, &version); err != nil {
		t.Fatal(err)
	}
	if status != "COMPLETED" || completedBy != "collector-1" || note != "已电话联系客户" || version != 2 {
		t.Fatalf("status=%s by=%s note=%s version=%d", status, completedBy, note, version)
	}
	var audits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_outbox_event WHERE tenant_id=? AND event_type='SETTLEMENT_DUNNING_ACTION_COMPLETED'`, tenant).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audits=%d err=%v", audits, err)
	}
	if err := service.CompleteDunningAction(context.Background(), Principal{TenantID: tenant, UserID: "other"}, "action-dunning-1", 1, "越权"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("wrong recipient err=%v, want ErrForbidden", err)
	}
	if _, err := db.Exec(`INSERT INTO settlement_dunning_action(id,tenant_id,dunning_case_id,action_type,recipient_user_id,channel,priority,event_id,status,version,sent_at,updated_at,created_at) VALUES('action-dunning-2',?, 'case-1','REMIND','collector-1','LOCAL','NORMAL','event-dunning-2','PENDING',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteDunningAction(context.Background(), collector, "action-dunning-2", 2, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version err=%v, want ErrConflict", err)
	}
}

func TestDunningOverviewOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_DUNNING_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_DUNNING_TEST_MYSQL_DSN is required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS settlement_dunning_case`,
		`DROP TABLE IF EXISTS settlement_dunning_action`,
		`DROP TABLE IF EXISTS settlement_dunning_policy`,
		`DROP TABLE IF EXISTS settlement_receivable`,
		`CREATE TABLE settlement_receivable (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,open_amount DECIMAL(18,2) NOT NULL,currency CHAR(3) NOT NULL,due_date DATE NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_dunning_policy (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,enabled BOOLEAN NOT NULL,aging_from_days INT NOT NULL,aging_to_days INT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_dunning_case (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,status VARCHAR(32) NOT NULL,current_escalation_level INT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_dunning_action (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,status VARCHAR(32) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tenant := "tenant-dunning-overview"
	if _, err := db.Exec(`INSERT INTO settlement_receivable(id,tenant_id,open_amount,currency,due_date) VALUES('r1',?,50.00,'CNY',DATE_SUB(CURDATE(),INTERVAL 10 DAY)),('r2',?,0.00,'CNY',DATE_SUB(CURDATE(),INTERVAL 20 DAY)),('r3',?,20.00,'USD',DATE_SUB(CURDATE(),INTERVAL 5 DAY))`, tenant, tenant, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settlement_dunning_policy(id,tenant_id,enabled,aging_from_days,aging_to_days) VALUES('p1',?,TRUE,1,90)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settlement_dunning_case(id,tenant_id,status,current_escalation_level) VALUES('c1',?,'ACTIVE',2)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settlement_dunning_action(id,tenant_id,status) VALUES('a1',?,'PENDING'),('a2',?,'COMPLETED')`, tenant, tenant); err != nil {
		t.Fatal(err)
	}
	overview, err := (&Service{DB: db}).DunningOverview(context.Background(), Principal{TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	if overview.OverdueCount != 2 || overview.OverdueAmount != "70.00" || overview.ActiveCaseCount != 1 || overview.EscalatedCaseCount != 1 || overview.ActionCount != 2 || overview.PendingActionCount != 1 || overview.UncoveredOverdue != 0 {
		t.Fatalf("overview=%+v", overview)
	}
	if len(overview.CurrencyTotals) != 2 || overview.CurrencyTotals[0].Currency != "CNY" || overview.CurrencyTotals[0].Overdue != "50.00" || overview.CurrencyTotals[1].Currency != "USD" || overview.CurrencyTotals[1].Overdue != "20.00" {
		t.Fatalf("currency totals=%+v", overview.CurrencyTotals)
	}
}
