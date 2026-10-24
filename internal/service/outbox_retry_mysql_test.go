package service

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestOutboxDeadLetterQueryAndRetryOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_DUNNING_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_DUNNING_TEST_MYSQL_DSN is required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`DROP TABLE IF EXISTS settlement_outbox_event`,
		`CREATE TABLE settlement_audit_event (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,actor_id VARCHAR(128) NOT NULL,action VARCHAR(128) NOT NULL,resource_type VARCHAR(64) NOT NULL,resource_id VARCHAR(128) NOT NULL,result VARCHAR(16) NOT NULL,reason_code VARCHAR(64) NOT NULL,request_id VARCHAR(64) NOT NULL,correlation_id VARCHAR(64) NOT NULL,detail_json JSON NOT NULL,occurred_at DATETIME(3) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_outbox_event (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,event_id CHAR(32) NOT NULL,destination VARCHAR(64) NOT NULL,event_type VARCHAR(128) NOT NULL,aggregate_type VARCHAR(64) NOT NULL,aggregate_id VARCHAR(128) NOT NULL,payload_json JSON NOT NULL,status VARCHAR(32) NOT NULL,attempt_count INT NOT NULL DEFAULT 0,available_at DATETIME(3) NOT NULL,locked_by VARCHAR(128) NULL,locked_until DATETIME(3) NULL,created_at DATETIME(3) NOT NULL,delivered_at DATETIME(3) NULL,last_error_code VARCHAR(128) NOT NULL DEFAULT '',last_error_summary VARCHAR(500) NOT NULL DEFAULT '',dead_lettered_at DATETIME(3) NULL,updated_at DATETIME(3) NOT NULL,UNIQUE KEY uq(tenant_id,destination,event_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	const tenant = "tenant-outbox"
	if _, err := db.Exec(`INSERT INTO settlement_outbox_event(id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,status,attempt_count,available_at,created_at,updated_at,last_error_code,last_error_summary,dead_lettered_at) VALUES('outbox-dead-1',?,'event-dead-1','PLATFORM_AUDIT','TEST','test','1','{}','DEAD_LETTER',7,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),'FAILED','terminal',UTC_TIMESTAMP(3))`, tenant); err != nil {
		t.Fatal(err)
	}
	p := Principal{TenantID: tenant, UserID: "admin", Permissions: map[string]bool{"settlement.admin": true}}
	items, err := (&Service{DB: db}).ListOutboxEvents(context.Background(), p)
	if err != nil || len(items) != 1 || items[0].Status != "DEAD_LETTER" || items[0].LastErrorCode != "FAILED" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if err := (&Service{DB: db}).RetryOutboxEvent(context.Background(), p, "outbox-dead-1"); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	if err := db.QueryRow(`SELECT status,attempt_count FROM settlement_outbox_event WHERE id='outbox-dead-1'`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING" || attempts != 0 {
		t.Fatalf("status=%s attempts=%d, want PENDING/0", status, attempts)
	}
	if err := (&Service{DB: db}).RetryOutboxEvent(context.Background(), p, "outbox-dead-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second retry err=%v, want ErrConflict", err)
	}
}
