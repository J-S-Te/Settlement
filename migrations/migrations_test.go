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
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("migrations missing %s", required)
		}
	}
}
