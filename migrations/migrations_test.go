package migrations

import (
	"strings"
	"testing"
)

func TestCoreMigrationProtectsFinancialFactsAndIdempotency(t *testing.T) {
	body, err := Files.ReadFile("000001_settlement_core.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"settlement_contract_snapshot", "settlement_receivable", "settlement_receipt_allocation", "settlement_receipt_allocation_reversal", "settlement_inbox_event", "settlement_outbox_event",
		"uq_settlement_contract_event", "uq_settlement_receipt_bank_reference", "uq_settlement_allocation_idempotency",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("migration missing %s", required)
		}
	}
	if strings.Contains(text, "FLOAT") || strings.Contains(text, "DOUBLE") {
		t.Fatal("financial migration must not use floating point columns")
	}
}

func TestAllMigrationsKeepIDAndControlPlaneContracts(t *testing.T) {
	entries, err := Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var combined strings.Builder
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := Files.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		combined.Write(body)
	}
	text := combined.String()
	if strings.Contains(text, "CHAR(26)") {
		t.Fatal("database IDs must match 32-character generated IDs")
	}
	for _, required := range []string{
		"settlement_oidc_session", "last_seen_at", "settlement_invoice_request_item",
		"settlement_tax_invoice", "settlement_dunning_policy", "settlement_local_notification",
		"locked_by", "locked_until", "dead_lettered_at", "settlement_contract_stream",
		"settlement_tax_result_inbox", "payload_hash", "processing_status",
		"settlement_invoice_red_flush_attempt", "settlement_invoice_allocation_reversal",
		"lease_generation", "settlement_report_export_effect", "uq_settlement_export_request",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("migrations missing %s", required)
		}
	}
}

func TestInvoiceDocumentAndRedFlushMigrationKeepsControlledWorkflow(t *testing.T) {
	body, err := Files.ReadFile("000009_invoice_documents_red_flush_requests.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{"original_name", "media_type", "match_confidence", "settlement_invoice_red_flush_request", "idempotency_key", "requested_by", "reviewed_by", "review_reason", "version", "status"} {
		if !strings.Contains(text, required) {
			t.Fatalf("invoice workflow migration missing %s", required)
		}
	}
}

// TestContractRevisionHoldMigrationIsIdempotentAndAuditable 验证合同财务修订工单的幂等与审计结构。
// 参数：t为测试上下文，工单应保留事件级唯一键及人工复核状态。
// 返回值：无；缺少守卫、唯一键、状态列或出现浮点金额时失败。
func TestContractRevisionHoldMigrationIsIdempotentAndAuditable(t *testing.T) {
	body, err := Files.ReadFile("000025_contract_revision_hold.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS settlement_contract_revision_hold",
		"uq_settlement_contract_revision_event", "revision_event_id", "hold_reason",
		"diff_json", "status", "resolution_reason", "resolved_by", "resolved_at",
		"idx_settlement_contract_revision_status",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("contract revision migration missing %s", required)
		}
	}
	if strings.Contains(text, "FLOAT") || strings.Contains(text, "DOUBLE") {
		t.Fatal("contract revision migration must not use floating point columns")
	}
}
