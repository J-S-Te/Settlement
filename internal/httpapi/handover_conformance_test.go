package httpapi

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/handover"
	"github.com/j-s-te/settlement/internal/platform"
)

// settlementConformanceAPI 用真实 SQL 仓储（隔离 MySQL）+ 真实服务构造 API。
func settlementConformanceAPI(t *testing.T, tenantID string) (*API, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("SETTLEMENT_HANDOVER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("SETTLEMENT_HANDOVER_TEST_MYSQL_DSN is not configured; no database connection attempted")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open disposable MySQL schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	statements := []string{
		"DROP TABLE IF EXISTS settlement_handover_receipt",
		"DROP TABLE IF EXISTS settlement_handover_freeze",
		"DROP TABLE IF EXISTS settlement_dunning_action",
		"CREATE TABLE settlement_handover_freeze (id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, batch_id CHAR(32) NOT NULL, request_id CHAR(32) NOT NULL, user_id VARCHAR(128) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'FROZEN', frozen_at DATETIME(3) NOT NULL, released_at DATETIME(3) NULL, outcome VARCHAR(16) NULL, created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL, PRIMARY KEY (id), UNIQUE KEY uk_settlement_handover_freeze_batch_user (tenant_id, batch_id, user_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE settlement_handover_receipt (id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, batch_id CHAR(32) NOT NULL, item_id CHAR(32) NOT NULL, resource_type VARCHAR(64) NOT NULL, resource_id VARCHAR(128) NOT NULL, from_user_id VARCHAR(128) NOT NULL, to_user_id VARCHAR(128) NOT NULL, idempotency_key VARCHAR(160) NOT NULL, status VARCHAR(24) NOT NULL, applied_version BIGINT UNSIGNED NOT NULL DEFAULT 0, reason_code VARCHAR(64) NULL, evidence_ref VARCHAR(255) NULL, received_at DATETIME(3) NOT NULL, created_at DATETIME(3) NOT NULL, PRIMARY KEY (id), UNIQUE KEY uk_settlement_handover_receipt_key (tenant_id, idempotency_key)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE settlement_dunning_action (id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, dunning_case_id CHAR(32) NOT NULL, action_type VARCHAR(32) NOT NULL, recipient_user_id VARCHAR(128) NOT NULL, channel VARCHAR(32) NOT NULL, priority VARCHAR(16) NOT NULL, event_id CHAR(32) NOT NULL, status VARCHAR(32) NOT NULL, sent_at DATETIME(3) NULL, result_detail VARCHAR(500) NOT NULL DEFAULT '', created_at DATETIME(3) NOT NULL, PRIMARY KEY (id), UNIQUE KEY uq_settlement_dunning_event (tenant_id, event_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("prepare disposable schema: %v", err)
		}
	}
	repository, err := handover.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	service, err := handover.NewService(repository, handover.NewID, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	api := &API{handover: service, handoverMachine: handoverVerifierStub{identity: platform.ServiceIdentity{TenantID: tenantID}},
		logger: slog.Default()}
	return api, db
}

// TestSettlementHandoverProtocolMatchesPlatformOrchestration 按平台编排器的真实请求序列走完流程。
func TestSettlementHandoverProtocolMatchesPlatformOrchestration(t *testing.T) {
	tenant := "tenant-conformance"
	api, db := settlementConformanceAPI(t, tenant)
	leaver, successor := "user-leaver", "user-successor"
	actionID := "act0000000000000000000000000001"
	if _, err := db.Exec("INSERT INTO settlement_dunning_action(id, tenant_id, dunning_case_id, action_type, recipient_user_id, channel, priority, event_id, status, created_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
		actionID, tenant, "case0000000000000000000000000001", "REMINDER", leaver, "INBOX", "HIGH", "evt00000000000000000000000000001", "PENDING", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	batchID, itemID := "bat0000000000000000000000000001", "itm0000000000000000000000000001"

	status, payload := handoverCall(t, api, api.handoverFreeze, http.MethodPost, "/internal/v1/handover/freeze",
		fmt.Sprintf(`{"batch_id":"%s","request_id":"%s","user_id":"%s"}`, batchID, itemID, leaver), "machine-token")
	if status != http.StatusOK {
		t.Fatalf("freeze → %d %v", status, payload)
	}
	data := payload["data"].(map[string]any)
	if data["system"] != "settlement" || data["frozen"] != true || data["responsibility_count"].(float64) != 1 {
		t.Fatalf("freeze payload = %v", payload)
	}

	status, payload = handoverCall(t, api, api.handoverResponsibilities, http.MethodGet,
		"/internal/v1/handover/responsibilities?user_id="+leaver+"&limit=10", "", "machine-token")
	if status != http.StatusOK {
		t.Fatalf("responsibilities → %d %v", status, payload)
	}
	items := payload["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("responsibilities = %v", items)
	}
	listed := items[0].(map[string]any)
	if listed["resource_id"] != actionID || listed["category"] != handover.CategoryDunningRecipient {
		t.Fatalf("listed responsibility = %v", listed)
	}

	transferBody := fmt.Sprintf(`{"batch_id":"%s","item_id":"%s","resource_type":"dunning_action","resource_id":"%s","from_user_id":"%s","to_user_id":"%s","expected_version":1,"idempotency_key":"handover:%s:%s"}`,
		batchID, itemID, actionID, leaver, successor, batchID, itemID)
	status, payload = handoverCall(t, api, api.handoverTransfer, http.MethodPost, "/internal/v1/handover/transfers", transferBody, "machine-token")
	if status != http.StatusOK {
		t.Fatalf("transfer → %d %v", status, payload)
	}
	receipt := payload["data"].(map[string]any)
	if receipt["status"] != handover.ReceiptApplied || receipt["evidence_ref"] == "" {
		t.Fatalf("receipt = %v", receipt)
	}
	var recipient string
	if err := db.QueryRow("SELECT recipient_user_id FROM settlement_dunning_action WHERE id = ?", actionID).Scan(&recipient); err != nil {
		t.Fatal(err)
	}
	if recipient != successor {
		t.Fatalf("recipient = %s, want successor", recipient)
	}

	// 重放：同回执，不改数据。
	status, payload = handoverCall(t, api, api.handoverTransfer, http.MethodPost, "/internal/v1/handover/transfers", transferBody, "machine-token")
	if status != http.StatusOK || payload["data"].(map[string]any)["status"] != handover.ReceiptApplied {
		t.Fatalf("replay → %d %v", status, payload)
	}

	// 已发送动作：守卫拒绝且不改数据。
	sentID := "act0000000000000000000000000002"
	if _, err := db.Exec("INSERT INTO settlement_dunning_action(id, tenant_id, dunning_case_id, action_type, recipient_user_id, channel, priority, event_id, status, created_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
		sentID, tenant, "case0000000000000000000000000001", "REMINDER", leaver, "INBOX", "HIGH", "evt00000000000000000000000000002", "SENT", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	sentBody := strings.Replace(transferBody, actionID, sentID, 1)
	sentBody = strings.Replace(sentBody, "handover:", "handover-sent:", 1)
	status, payload = handoverCall(t, api, api.handoverTransfer, http.MethodPost, "/internal/v1/handover/transfers", sentBody, "machine-token")
	if status != http.StatusOK {
		t.Fatalf("sent transfer → %d %v", status, payload)
	}
	rejected := payload["data"].(map[string]any)
	if rejected["status"] != handover.ReceiptRejected || rejected["reason_code"] != "SETTLEMENT_ACTION_NOT_PENDING" {
		t.Fatalf("sent receipt = %v", rejected)
	}
	if err := db.QueryRow("SELECT recipient_user_id FROM settlement_dunning_action WHERE id = ?", sentID).Scan(&recipient); err != nil {
		t.Fatal(err)
	}
	if recipient != leaver {
		t.Fatalf("rejected transfer must not change the recipient, got %s", recipient)
	}

	// 完成：解冻，已完成转移保留。
	status, payload = handoverCall(t, api, api.handoverComplete, http.MethodPost, "/internal/v1/handover/complete",
		fmt.Sprintf(`{"batch_id":"%s","outcome":"COMPLETED"}`, batchID), "machine-token")
	if status != http.StatusOK {
		t.Fatalf("complete → %d %v", status, payload)
	}
	var freezeStatus string
	if err := db.QueryRow("SELECT status FROM settlement_handover_freeze WHERE tenant_id = ? AND batch_id = ?", tenant, batchID).Scan(&freezeStatus); err != nil {
		t.Fatal(err)
	}
	if freezeStatus != handover.FreezeReleased {
		t.Fatalf("freeze status = %s", freezeStatus)
	}
	if err := db.QueryRow("SELECT recipient_user_id FROM settlement_dunning_action WHERE id = ?", actionID).Scan(&recipient); err != nil {
		t.Fatal(err)
	}
	if recipient != successor {
		t.Fatalf("completed transfer must be preserved, recipient = %s", recipient)
	}
}
