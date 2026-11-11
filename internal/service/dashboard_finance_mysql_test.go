package service

import (
	"context"
	"database/sql"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/migration"
	"github.com/j-s-te/settlement/migrations"
)

// dashboardFinanceMySQL 打开显式文件绑定的回环测试库并执行真实迁移。
// 参数：t为测试上下文，只读取SETTLEMENT_DASHBOARD_TEST_DSN_FILE。
// 返回值：隔离SQL连接；未配置则跳过，路径及数据库边界非法则失败。
func dashboardFinanceMySQL(t *testing.T) *sql.DB {
	t.Helper()
	path := os.Getenv("SETTLEMENT_DASHBOARD_TEST_DSN_FILE")
	if path == "" {
		t.Skip("SETTLEMENT_DASHBOARD_TEST_DSN_FILE required; no business database accessed")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private regular DSN file required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read test DSN failed")
	}
	dsn := strings.TrimSpace(string(raw))
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || host != "127.0.0.1" || !strings.HasPrefix(cfg.DBName, "settlement_test_") {
		t.Fatal("explicit loopback settlement_test_* database required")
	}
	if err = migration.Run(t.Context(), dsn, migrations.Files); err != nil {
		t.Fatal("isolated migrations failed")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open isolated test database failed")
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// dashboardFinanceSeed 插入独立测试租户的最小财务事实。
// 参数：t为测试上下文，db为隔离库，tenant为合成租户，id/currency/state定义快照。
// 返回值：无，合成事实构造失败即终止测试。
func dashboardFinanceSeed(t *testing.T, db *sql.DB, tenant, id, currency, state string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO settlement_contract_snapshot
 (id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,
 source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,
 financial_status,effective_at,created_at,updated_at,migration_state)
 VALUES(?,?,'qa',?,?,?,1,'customer','QA',?,'100.00','EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),?)`, id, tenant, id, id, id, currency, state)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO settlement_receivable
 (id,tenant_id,receivable_no,contract_snapshot_id,due_date,original_amount,received_allocated_amount,
 write_off_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at)
 VALUES(?,?,?,?, '2026-08-02','100.00','20.00','30.00','50.00',?,'CONFIRMED','PARTIALLY_SETTLED','NOT_INVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, id, id, currency)
	if err != nil {
		t.Fatal(err)
	}
}

// TestDashboardFinanceMySQL 验证已释放、净冲销、核销、有效回款、多币种及北京时间。
// 参数：t为测试上下文，数据库仅由显式隔离DSN文件指定。
// 返回值：无，实际聚合SQL口径、精度或租户边界错误时失败。
func TestDashboardFinanceMySQL(t *testing.T) {
	db := dashboardFinanceMySQL(t)
	// 纳秒时间加不同前缀生成独立合成身份，不改动既有租户数据。
	suffix := time.Now().UTC().Format("150405.000000000")
	tenant := "dashboard-" + suffix
	service := &Service{DB: db}
	now := time.Date(2026, 10, 31, 16, 1, 0, 0, time.UTC)
	empty, err := service.DashboardFinance(context.Background(), tenant, now)
	if err != nil || !empty.Available || len(empty.Metrics.Currencies) != 0 || empty.SourceUpdatedAt != "" {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	for _, row := range []struct{ id, currency, state string }{{"cny-", "CNY", "RELEASED"}, {"usd-", "USD", "RELEASED"}, {"hold-", "CNY", "HOLD"}} {
		dashboardFinanceSeed(t, db, tenant, row.id+suffix, row.currency, row.state)
	}
	dashboardFinanceSeed(t, db, tenant+"other", "other-"+suffix, "CNY", "RELEASED")
	if _, err = db.Exec(`UPDATE settlement_receivable SET due_date='2026-08-03' WHERE tenant_id=? AND id=?`, tenant, "usd-"+suffix); err != nil {
		t.Fatal(err)
	}
	// 旧allocation台账故意保留50元；净已核销20元才是看板来源，不重复累计旧记录。
	for i, entry := range []struct{ id, status, date, amount, currency string }{{"receipt-", "PARTIALLY_ALLOCATED", "2026-11-01", "100.00", "CNY"}, {"heldreceipt-", "AVAILABLE", "2026-11-01", "999.00", "CNY"}, {"invalidreceipt-", "VOID", "2026-11-01", "999.00", "CNY"}, {"oldreceipt-", "AVAILABLE", "2026-10-31", "999.00", "CNY"}, {"usdreceipt-", "PARTIALLY_ALLOCATED", "2026-11-01", "0.01", "USD"}} {
		id := entry.id + suffix
		_, err = db.Exec(`INSERT INTO settlement_receipt (id,tenant_id,receipt_no,customer_id,customer_name_snapshot,amount,unallocated_amount,currency,receipt_date,payment_method,bank_transaction_reference,source_type,status,idempotency_key,created_at,updated_at)
 VALUES(?,?,?,'customer','QA',?,?,?,?,'BANK',?,'MANUAL',?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, id, entry.amount, entry.amount, entry.currency, entry.date, id, entry.status, id)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			_, err = db.Exec(`INSERT INTO settlement_import_fact (tenant_id,contract_snapshot_id,source_system,fact_type,external_fact_id,target_business_id,request_hash,created_at) VALUES(?,?,'qa','RECEIPT',?,?,?,UTC_TIMESTAMP(3))`, tenant, "hold-"+suffix, id, id, strings.Repeat("a", 32))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	_, err = db.Exec(`INSERT INTO settlement_receipt_allocation (id,tenant_id,allocation_no,receipt_id,receivable_id,allocated_amount,currency,status,match_mode,confirmed_by,confirmed_at,idempotency_key,created_at) VALUES(?,?,?,?,?,'50.00','CNY','CONFIRMED','MANUAL','qa',UTC_TIMESTAMP(3),?,UTC_TIMESTAMP(3))`, "allocation-"+suffix, tenant, "allocation-"+suffix, "receipt-"+suffix, "cny-"+suffix, "allocation-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO settlement_receipt_allocation_reversal
 (id,tenant_id,reversal_no,original_allocation_id,reversed_amount,reason_code,reason_detail,approved_by,reversed_at,idempotency_key)
 VALUES(?,?,?,?,'30.00','QA','合成冲销','qa',UTC_TIMESTAMP(3),?)`, "reversal-"+suffix, tenant, "reversal-"+suffix, "allocation-"+suffix, "reversal-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE settlement_receipt SET unallocated_amount='80.00' WHERE tenant_id=? AND id=?`, tenant, "receipt-"+suffix); err != nil {
		t.Fatal(err)
	}
	got, err := service.DashboardFinance(context.Background(), tenant, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metrics.Month != "2026-11" || got.Metrics.AsOfDate != "2026-11-01" || len(got.Metrics.Currencies) != 2 || got.SourceUpdatedAt == "" {
		t.Fatalf("snapshot=%+v", got)
	}
	for _, row := range got.Metrics.Currencies {
		wantReceipt, wantOver90 := int64(10000), int64(5000)
		if row.Currency == "USD" {
			wantReceipt = 1
			wantOver90 = 0
		}
		if row.OriginalMinor != 10000 || row.AllocatedMinor != 2000 || row.OpenMinor != 5000 || row.Over90Minor != wantOver90 || row.MonthReceiptsMinor != wantReceipt || row.CollectionRate == nil || *row.CollectionRate != 20 {
			t.Fatalf("currency=%+v", row)
		}
	}
}
