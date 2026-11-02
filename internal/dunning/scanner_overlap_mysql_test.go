package dunning

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// TestDunningOverlappingPoliciesKeepOneActiveCaseOnMySQL 验证重叠策略在真实
// MySQL 上不会为同一应收创建第二个 ACTIVE 案件：既有案件继续作为唯一权威案件。
// 参数：t 测试上下文，仅在显式 SETTLEMENT_DUNNING_TEST_MYSQL_DSN 时运行。
// 返回值：无；重复案件或跳过既有案件时失败。
func TestDunningOverlappingPoliciesKeepOneActiveCaseOnMySQL(t *testing.T) {
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
		`DROP TABLE IF EXISTS settlement_contract_import`,
		`DROP TABLE IF EXISTS settlement_contract_responsibility`,
		`DROP TABLE IF EXISTS settlement_contract_snapshot`,
		`CREATE TABLE settlement_contract_snapshot (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,customer_name_snapshot VARCHAR(255) NOT NULL,source_contract_id CHAR(32) NOT NULL DEFAULT '',financial_status VARCHAR(32) NOT NULL DEFAULT 'EFFECTIVE',migration_state VARCHAR(32) NOT NULL DEFAULT 'RELEASED') ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_contract_import (tenant_id VARCHAR(64) NOT NULL,contract_snapshot_id CHAR(32) NOT NULL,release_version BIGINT UNSIGNED NOT NULL DEFAULT 0,PRIMARY KEY (tenant_id,contract_snapshot_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_contract_responsibility (tenant_id VARCHAR(64) NOT NULL,source_contract_id CHAR(32) NOT NULL,owner_user_id VARCHAR(128) NOT NULL DEFAULT '',active BOOLEAN NOT NULL DEFAULT TRUE) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_receivable (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,receivable_no VARCHAR(64) NOT NULL,contract_snapshot_id CHAR(32) NOT NULL,receivable_plan_id CHAR(32) NULL,due_date DATE NOT NULL,open_amount DECIMAL(18,2) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_dunning_policy (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,name VARCHAR(128) NOT NULL,version INT NOT NULL,aging_from_days INT NOT NULL,aging_to_days INT NOT NULL,action_type VARCHAR(32) NOT NULL,recipient_rule VARCHAR(128) NOT NULL,channel VARCHAR(32) NOT NULL,repeat_interval_days INT NOT NULL,priority VARCHAR(16) NOT NULL,enabled BOOLEAN NOT NULL,created_at DATETIME(3) NOT NULL,updated_at DATETIME(3) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_dunning_case (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,receivable_id CHAR(32) NOT NULL,plan_id CHAR(32) NULL,dunning_policy_id CHAR(32) NOT NULL,policy_version INT NOT NULL,current_escalation_level INT NOT NULL DEFAULT 0,status VARCHAR(32) NOT NULL,next_action_at DATETIME(3) NOT NULL,last_action_at DATETIME(3) NULL,closed_reason VARCHAR(255) NOT NULL DEFAULT '',closed_at DATETIME(3) NULL,created_at DATETIME(3) NOT NULL,updated_at DATETIME(3) NOT NULL,UNIQUE KEY uq_case(tenant_id,receivable_id,dunning_policy_id,policy_version)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
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
	const tenant = "tenant-dunning-overlap"
	for _, q := range []string{
		`INSERT INTO settlement_contract_snapshot(id,tenant_id,customer_name_snapshot) VALUES('overlap-snapshot',?,'重叠客户')`,
	} {
		if _, err := db.Exec(q, tenant); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,due_date,open_amount) VALUES('overlap-receivable',?,'YS-OVERLAP','overlap-snapshot',DATE_SUB(CURDATE(),INTERVAL 5 DAY),100.00)`, tenant); err != nil {
		t.Fatal(err)
	}
	insertPolicy := func(id, name string, from, to int) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO settlement_dunning_policy(id,tenant_id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled,created_at,updated_at) VALUES(?,?,?,1,?,?,'REMIND','USER:collector-1','LOCAL',7,'NORMAL',TRUE,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, name, from, to); err != nil {
			t.Fatal(err)
		}
	}
	insertPolicy("overlap-policy-a", "首轮策略", 1, 30)
	scanner := &Scanner{DB: db, BatchSize: 50, Directory: collectorStub{eligible: true}}
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var cases int
	var policyID string
	if err := db.QueryRow(`SELECT COUNT(*),MIN(dunning_policy_id) FROM settlement_dunning_case WHERE tenant_id=? AND status='ACTIVE'`, tenant).Scan(&cases, &policyID); err != nil || cases != 1 || policyID != "overlap-policy-a" {
		t.Fatalf("initial case count=%d policy=%s err=%v", cases, policyID, err)
	}
	// 第二轮出现账龄区间更宽的重叠策略；候选查询会选中它，但既有 ACTIVE
	// 案件必须继续作为同一催收义务的唯一案件。
	insertPolicy("overlap-policy-b", "重叠策略", 2, 60)
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_case WHERE tenant_id=? AND status='ACTIVE'`, tenant).Scan(&cases); err != nil || cases != 1 {
		t.Fatalf("overlapping policy created duplicate ACTIVE case: cases=%d err=%v", cases, err)
	}
}
