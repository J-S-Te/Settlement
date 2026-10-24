package handover

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func settlementRepositoryFor(t *testing.T) (*SQLRepository, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("SETTLEMENT_HANDOVER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("SETTLEMENT_HANDOVER_TEST_MYSQL_DSN is not configured")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open disposable MySQL schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	statements := []string{
		`DROP TABLE IF EXISTS settlement_handover_receipt`,
		`DROP TABLE IF EXISTS settlement_handover_freeze`,
		`DROP TABLE IF EXISTS settlement_dunning_action`,
		`CREATE TABLE settlement_handover_freeze (id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, batch_id CHAR(32) NOT NULL, request_id CHAR(32) NOT NULL, user_id VARCHAR(128) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'FROZEN', frozen_at DATETIME(3) NOT NULL, released_at DATETIME(3) NULL, outcome VARCHAR(16) NULL, created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL, PRIMARY KEY (id), UNIQUE KEY uk_settlement_handover_freeze_batch_user (tenant_id, batch_id, user_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_handover_receipt (id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, batch_id CHAR(32) NOT NULL, item_id CHAR(32) NOT NULL, resource_type VARCHAR(64) NOT NULL, resource_id VARCHAR(128) NOT NULL, from_user_id VARCHAR(128) NOT NULL, to_user_id VARCHAR(128) NOT NULL, idempotency_key VARCHAR(160) NOT NULL, status VARCHAR(24) NOT NULL, applied_version BIGINT UNSIGNED NOT NULL DEFAULT 0, reason_code VARCHAR(64) NULL, evidence_ref VARCHAR(255) NULL, received_at DATETIME(3) NOT NULL, created_at DATETIME(3) NOT NULL, PRIMARY KEY (id), UNIQUE KEY uk_settlement_handover_receipt_key (tenant_id, idempotency_key)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE settlement_dunning_action (id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, dunning_case_id CHAR(32) NOT NULL, action_type VARCHAR(32) NOT NULL, recipient_user_id VARCHAR(128) NOT NULL, channel VARCHAR(32) NOT NULL, priority VARCHAR(16) NOT NULL, event_id CHAR(32) NOT NULL, status VARCHAR(32) NOT NULL, sent_at DATETIME(3) NULL, result_detail VARCHAR(500) NOT NULL DEFAULT '', created_at DATETIME(3) NOT NULL, PRIMARY KEY (id), UNIQUE KEY uq_settlement_dunning_event (tenant_id, event_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("prepare disposable schema: %v", err)
		}
	}
	repository, err := NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	return repository, db
}

func seedDunningAction(t *testing.T, db *sql.DB, tenantID, id, recipient, status string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Exec("INSERT INTO settlement_dunning_action(id, tenant_id, dunning_case_id, action_type, recipient_user_id, channel, priority, event_id, status, created_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
		id, tenantID, "case0000000000000000000000000001", "REMINDER", recipient, "INBOX", "HIGH", "ev"+id[2:], status, now); err != nil {
		t.Fatalf("seed dunning action: %v", err)
	}
}

// 只有"本人 + PENDING"的催收动作计入责任。
func TestSettlementResponsibilitiesOnMySQL(t *testing.T) {
	repository, db := settlementRepositoryFor(t)
	ctx := context.Background()
	tenant, leaver := "tenant-st-1", "user-leaver"
	seedDunningAction(t, db, tenant, "act-0000000000000000000000000001", leaver, "PENDING")
	seedDunningAction(t, db, tenant, "act-0000000000000000000000000002", leaver, "SENT")
	seedDunningAction(t, db, tenant, "act-0000000000000000000000000003", "user-other", "PENDING")
	seedDunningAction(t, db, "tenant-st-other", "act-0000000000000000000000000004", leaver, "PENDING")

	items, err := repository.ListResponsibilities(ctx, tenant, leaver, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ResourceID != "act-0000000000000000000000000001" || items[0].Category != CategoryDunningRecipient {
		t.Fatalf("responsibilities = %+v", items)
	}
}

// 转移改派收件人；对已发送（非 PENDING）的动作必须 REJECTED 且不改数据。
func TestSettlementTransferOwnershipOnMySQL(t *testing.T) {
	repository, db := settlementRepositoryFor(t)
	ctx := context.Background()
	tenant, leaver, successor := "tenant-st-2", "user-leaver", "user-successor"
	seedDunningAction(t, db, tenant, "act-0000000000000000000000000011", leaver, "PENDING")

	input := TransferInput{TenantID: tenant, BatchID: "batch-00000000000000000000000001", ItemID: "item-000000000000000000000000001",
		ResourceType: ResourceDunningAction, ResourceID: "act-0000000000000000000000000011",
		FromUserID: leaver, ToUserID: successor, ExpectedVersion: 1, IdempotencyKey: "handover:st-batch:item-1"}
	receipt, err := repository.TransferOwnership(ctx, input, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != ReceiptApplied || receipt.EvidenceRef == "" {
		t.Fatalf("receipt = %+v", receipt)
	}
	var recipient string
	if err := db.QueryRow("SELECT recipient_user_id FROM settlement_dunning_action WHERE id = ?", input.ResourceID).Scan(&recipient); err != nil {
		t.Fatal(err)
	}
	if recipient != successor {
		t.Fatalf("recipient = %s, want successor", recipient)
	}

	replayed, err := repository.TransferOwnership(ctx, input, time.Now().UTC())
	if err != nil || replayed.Status != ReceiptApplied {
		t.Fatalf("replay = %+v err = %v", replayed, err)
	}

	// 已发送动作：守卫应拒绝，且不改数据。
	seedDunningAction(t, db, tenant, "act-0000000000000000000000000012", leaver, "SENT")
	sent := input
	sent.ResourceID, sent.IdempotencyKey = "act-0000000000000000000000000012", "handover:st-batch:item-2"
	rejected, err := repository.TransferOwnership(ctx, sent, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != ReceiptRejected || rejected.ReasonCode != stateChangedReason {
		t.Fatalf("sent action receipt = %+v", rejected)
	}
	if err := db.QueryRow("SELECT recipient_user_id FROM settlement_dunning_action WHERE id = ?", sent.ResourceID).Scan(&recipient); err != nil {
		t.Fatal(err)
	}
	if recipient != leaver {
		t.Fatalf("rejected transfer must not change the recipient, got %s", recipient)
	}
}
