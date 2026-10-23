package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/httpapi"
)

// TestDashboardCurrencyTotalsOnMySQL 验证看板在 SQL 中聚合整个
// 租户并分离币种，包含超过 50 行的场景。
func TestDashboardCurrencyTotalsOnMySQL(t *testing.T) {
	if os.Getenv("SETTLEMENT_E2E_TEST") != "1" {
		t.Skip("set SETTLEMENT_E2E_TEST=1 to run the database-backed dashboard flow")
	}
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_E2E_TEST_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("SETTLEMENT_MYSQL_DSN"))
	}
	if dsn == "" {
		t.Fatal("SETTLEMENT_E2E_TEST_DSN or SETTLEMENT_MYSQL_DSN is required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("database is not reachable: %v", err)
	}
	const tenant = "dev"
	cleanup := func() {
		for _, q := range []string{
			`DELETE FROM settlement_invoice_request WHERE tenant_id=?`,
			`DELETE FROM settlement_receipt WHERE tenant_id=?`,
			`DELETE FROM settlement_receivable WHERE tenant_id=?`,
			`DELETE FROM settlement_contract_snapshot WHERE tenant_id=?`,
		} {
			if _, err := db.Exec(q, tenant); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	}
	cleanup()
	t.Cleanup(func() { cleanup(); _ = db.Close() })

	now := time.Now().UTC()
	cnContract, usContract := "GLC00000000000000000000000000001", "GLC00000000000000000000000000002"
	for _, c := range []struct {
		id, currency string
		amount       string
	}{{cnContract, "CNY", "100000.00"}, {usContract, "USD", "100000.00"}} {
		if _, err := db.Exec(`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,project_id,project_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, c.id, tenant, "integration-test", c.id, "contract-"+c.id, "HT-"+c.id, 1, "customer-dashboard", "Dashboard customer", "project-dashboard", "Dashboard project", c.currency, c.amount); err != nil {
			t.Fatal(err)
		}
	}
	due := now.AddDate(0, 0, -1).Format("2006-01-02")
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("GLR%029d", i)
		if _, err := db.Exec(`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,due_date,original_amount,invoiced_amount,received_allocated_amount,write_off_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,?,?,?,?,10,0,0,0,10,?,'CONFIRMED','OVERDUE','NOT_INVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, fmt.Sprintf("YS-DASH-%03d", i), cnContract, due, "CNY"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("GLU%029d", i)
		if _, err := db.Exec(`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,due_date,original_amount,invoiced_amount,received_allocated_amount,write_off_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,?,?,?,?,20,0,0,0,20,?,'CONFIRMED','OVERDUE','NOT_INVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, fmt.Sprintf("YS-USD-%03d", i), usContract, due, "USD"); err != nil {
			t.Fatal(err)
		}
	}
	for _, inv := range []struct{ id, contract, currency, amount string }{
		{"GLI00000000000000000000000000001", cnContract, "CNY", "100.00"},
		{"GLI00000000000000000000000000002", usContract, "USD", "50.00"},
	} {
		if _, err := db.Exec(`INSERT INTO settlement_invoice_request(id,tenant_id,request_no,contract_snapshot_id,buyer_profile_snapshot,invoice_type,amount_excl_tax,tax_amount,amount_incl_tax,status,idempotency_key,submitted_by,created_at,updated_at) VALUES(?,?,?,?,?,'SPECIAL',?,?,?,'SUBMITTED',?,'tester',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, inv.id, tenant, "IR-"+inv.id, inv.contract, "{}", inv.amount, "0.00", inv.amount, "idem-"+inv.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, rec := range []struct{ id, currency, amount string }{
		{"GLP00000000000000000000000000001", "CNY", "30.00"},
		{"GLP00000000000000000000000000002", "USD", "20.00"},
	} {
		if _, err := db.Exec(`INSERT INTO settlement_receipt(id,tenant_id,receipt_no,customer_id,customer_name_snapshot,amount,unallocated_amount,currency,receipt_date,payment_method,bank_transaction_reference,source_type,status,idempotency_key,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'BANK',?,'MANUAL','AVAILABLE',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, rec.id, tenant, "SK-"+rec.id, "customer-dashboard", "Dashboard customer", rec.amount, rec.amount, rec.currency, now.Format("2006-01-02"), "bank-"+rec.id, "idem-"+rec.id); err != nil {
			t.Fatal(err)
		}
	}

	handler := httpapi.New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "http://localhost:5173"}, slog.Default(), nil, nil, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data struct {
			CurrencyTotals []map[string]string `json:"currency_totals"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]string{}
	for _, row := range envelope.Data.CurrencyTotals {
		got[row["currency"]] = row
	}
	for currency, want := range map[string]map[string]string{
		"CNY": {"open_receivable": "600.00", "overdue": "600.00", "current_month_receipts": "30.00", "pending_invoice": "100.00"},
		"USD": {"open_receivable": "100.00", "overdue": "100.00", "current_month_receipts": "20.00", "pending_invoice": "50.00"},
	} {
		for field, value := range want {
			if got[currency][field] != value {
				t.Errorf("%s %s=%q want %q (rows=%+v)", currency, field, got[currency][field], value, got)
			}
		}
	}
}
