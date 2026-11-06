package service

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"regexp"
	"strconv"
	"sync"
	"testing"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/migration"
	"github.com/j-s-te/settlement/migrations"
)

// nativeFinancialMySQL 仅在明确随机空库中运行真实迁移，不寻找业务数据库。
// 参数：t为测试上下文，DSN必须通过SETTLEMENT_NATIVE_TEST_DSN显式传入。
// 返回值：已迁移的隔离连接；缺DSN跳过，边界或迁移错误失败。
func nativeFinancialMySQL(t *testing.T) *sql.DB {
	t.Helper()
	raw := os.Getenv("SETTLEMENT_NATIVE_TEST_DSN")
	if raw == "" {
		t.Skip("requires disposable SETTLEMENT_NATIVE_TEST_DSN; no connection attempted")
	}
	cfg, err := driver.ParseDSN(raw)
	if err != nil {
		t.Fatal("invalid isolated test DSN")
	}
	host, portRaw, err := net.SplitHostPort(cfg.Addr)
	port, portErr := strconv.Atoi(portRaw)
	if err != nil || portErr != nil || cfg.Net != "tcp" || host != "127.0.0.1" || port < 32768 || !regexp.MustCompile(`^settlement_native_test_[a-f0-9]{8}$`).MatchString(cfg.DBName) {
		t.Fatal("requires uniquely named loopback high-port test schema")
	}
	db, err := sql.Open("mysql", raw)
	if err != nil {
		t.Fatal("open isolated test database failed")
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	db.SetMaxOpenConns(4)
	var tables int
	if err := db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables); err != nil || tables != 0 {
		t.Fatal("requires a new empty isolated schema")
	}
	for range 2 {
		if err := migration.Run(context.Background(), raw, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// TestNativeFinancialCoreMySQL 验证独立财务依据经真实计划、应收、回款和核销闭环。
// 参数：t为测试上下文，全部数据为本轮新空库内合成数据。
// 返回值：无；跨租户、幂等、精确金额、来源或核销不符合业务约束时失败。
func TestNativeFinancialCoreMySQL(t *testing.T) {
	db := nativeFinancialMySQL(t)
	s := &Service{DB: db}
	s.ConfigureFinancialSources(true, false)
	p := nativeTestPrincipal("settlement_finance_manager")
	p.Permissions["settlement.receivable.confirm"] = true
	accountant := nativeTestPrincipal("settlement_accountant")
	accountant.UserID = "accountant-b"
	accountant.Permissions = map[string]bool{
		"settlement.receivable.read": true, "settlement.receivable.confirm": true,
		"settlement.receipt.record": true, "settlement.allocation.confirm": true,
	}
	ctx := context.Background()
	customer, err := s.CreateNativeFinancialCustomer(ctx, p, "native-customer", NativeCustomerInput{Name: "QA独立财务客户"})
	if err != nil {
		t.Fatal(err)
	}
	input := nativeTestInput()
	input.CustomerKey = customer.CustomerKey
	input.PaymentTerms = []PaymentTerm{{InstallmentNo: 1, DueDate: "2026-11-05", Amount: "100.00"}}
	var wg sync.WaitGroup
	results := make(chan NativeFinancialBaseline, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			copyInput := input
			copyInput.PaymentTerms = append([]PaymentTerm(nil), input.PaymentTerms...)
			value, err := s.CreateNativeFinancialBaseline(ctx, p, "same-native-baseline", copyInput)
			results <- value
			failures <- err
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var baseline NativeFinancialBaseline
	for value := range results {
		if baseline.ID != "" && baseline.ID != value.ID {
			t.Fatal("same command created duplicate financial baselines")
		}
		baseline = value
	}
	var count int
	var source sql.NullString
	var sourceNo, basis string
	if err := db.QueryRow(`SELECT COUNT(*),MAX(source_contract_id),MAX(source_contract_no),MAX(basis_type) FROM settlement_contract_snapshot WHERE tenant_id=?`, p.TenantID).Scan(&count, &source, &sourceNo, &basis); err != nil || count != 1 || source.Valid || sourceNo != "" || basis != NativeFinancialBasis {
		t.Fatalf("source identity was fabricated: count=%d source_valid=%v basis=%s err=%v", count, source.Valid, basis, err)
	}
	foreign := p
	foreign.TenantID = "another-isolated-tenant"
	if _, err := s.GetNativeFinancialBaseline(ctx, foreign, baseline.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross tenant baseline: %v", err)
	}
	var plan string
	var version int
	if err := db.QueryRow(`SELECT id,version FROM settlement_receivable_plan WHERE tenant_id=? AND contract_snapshot_id=?`, p.TenantID, baseline.ID).Scan(&plan, &version); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmPlan(ctx, p, plan, PlanConfirmationInput{Version: version}); err != nil {
		t.Fatal(err)
	}
	var receivable string
	if err := db.QueryRow(`SELECT id FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=?`, p.TenantID, baseline.ID).Scan(&receivable); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.CreateReceipt(ctx, accountant, "native-receipt", ReceiptInput{CustomerID: customer.CustomerKey, CustomerName: customer.Name, Amount: "100.00", Currency: "CNY", ReceiptDate: "2026-11-04", PaymentMethod: "TRANSFER", BankReference: "QA-NATIVE-BANK-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmAllocation(ctx, accountant, "native-allocation", AllocationInput{ReceiptID: receipt, ReceivableID: receivable, Amount: "100.00", MatchMode: "MANUAL"}); err != nil {
		t.Fatal(err)
	}
	var open, unallocated string
	if err := db.QueryRow(`SELECT r.open_amount,receipt.unallocated_amount FROM settlement_receivable r JOIN settlement_receipt receipt ON receipt.id=? AND receipt.tenant_id=r.tenant_id WHERE r.id=? AND r.tenant_id=?`, receipt, receivable, p.TenantID).Scan(&open, &unallocated); err != nil || open != "0.00" || unallocated != "0.00" {
		t.Fatalf("money not settled exactly: open=%s unallocated=%s err=%v", open, unallocated, err)
	}
	if err := s.VoidNativeFinancialBaseline(ctx, p, baseline.ID, "void-consumed", NativeFinancialVoidInput{Version: 1, Reason: "测试已消费拒绝"}); !errors.Is(err, ErrNativeFinancialConsumed) {
		t.Fatalf("consumed baseline was voided: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_credit_sync WHERE tenant_id=?`, p.TenantID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unsubscribed credit emitted: count=%d err=%v", count, err)
	}
}
