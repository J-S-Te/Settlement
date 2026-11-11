package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
	"strings"
	"testing"
	"time"
)

// TestDashboardFinancePreservesExactMinorAndBeijingBoundaries 验证金额与日历边界。
// 参数：t为测试上下文。
// 返回值：无，精度、租户、事务或业务日历漂移时失败。
func TestDashboardFinancePreservesExactMinorAndBeijingBoundaries(t *testing.T) {
	var args, receiptArgs []driver.NamedValue
	var query, receiptQuery string
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable r", Args: &args, SQLOut: &query, Rows: [][]driver.Value{{"CNY", "90071992547409.93", "10.01", "5.02", "2.03", []byte("2026-10-31 00:00:00")}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receipt rc", Args: &receiptArgs, SQLOut: &receiptQuery, Rows: [][]driver.Value{{"CNY", "0.17", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}, {"USD", "1.01", nil}}},
		scriptdb.Step{Kind: "commit"},
	)
	got, err := (&Service{DB: db}).DashboardFinance(context.Background(), "tenant", time.Date(2026, 10, 31, 16, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	rows := got.Metrics.Currencies
	if !got.Available || got.TenantID != "tenant" || got.Metrics.Month != "2026-11" || got.Metrics.AsOfDate != "2026-11-01" || len(rows) != 2 || rows[0].OriginalMinor != 9007199254740993 || rows[0].AllocatedMinor != 1001 || rows[0].OpenMinor != 502 || rows[0].Over90Minor != 203 || rows[0].MonthReceiptsMinor != 17 || rows[1].CollectionRate != nil {
		t.Fatalf("snapshot=%+v metrics=%+v", got, got.Metrics)
	}
	if len(args) != 2 || args[0].Value != "2026-08-03" || args[1].Value != "tenant" || len(receiptArgs) != 3 || receiptArgs[0].Value != "tenant" || receiptArgs[1].Value != "2026-11-01" || receiptArgs[2].Value != "2026-12-01" {
		t.Fatalf("args=%v receipts=%v", args, receiptArgs)
	}
	if !strings.Contains(query, "cs.tenant_id=r.tenant_id") || !strings.Contains(query, "cs.migration_state='RELEASED'") || !strings.Contains(query, "r.recognition_status='CONFIRMED'") || strings.Contains(query, "settlement_receipt_allocation") {
		t.Fatal("receivable tenant/release/net-allocation boundary changed")
	}
	if !strings.Contains(receiptQuery, "f.tenant_id=rc.tenant_id") || !strings.Contains(receiptQuery, "cs.tenant_id=f.tenant_id") || !strings.Contains(receiptQuery, "cs.migration_state <> 'RELEASED'") || !strings.Contains(receiptQuery, "'AVAILABLE','PARTIALLY_ALLOCATED','FULLY_ALLOCATED'") {
		t.Fatal("receipt tenant/release/status boundary changed")
	}
}

// TestDashboardMinorRejectsPrecisionLossAndOverflow 验证金额转换失败关闭。
// 参数：t为测试上下文。
// 返回值：无，舍入、浮点误差或整数溢出被接受时失败。
func TestDashboardMinorRejectsPrecisionLossAndOverflow(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
		bad   bool
	}{
		{"0.00", 0, false}, {"0.01", 1, false}, {"-0.01", -1, false},
		{"92233720368547758.07", 9223372036854775807, false},
		{"92233720368547758.08", 0, true}, {"0.001", 0, true}, {"NaN", 0, true}, {"0x10", 0, true}, {"1/2", 0, true}, {"+1.00", 0, true},
	} {
		got, err := dashboardMinor(tc.value)
		if (err != nil) != tc.bad || !tc.bad && got != tc.want {
			t.Errorf("%s=%d err=%v", tc.value, got, err)
		}
	}
}

// TestDashboardFinanceDoesNotPublishFailedCommit 验证提交失败不输出成功快照。
// 参数：t为测试上下文。
// 返回值：无，源事务失败被吞没时失败。
func TestDashboardFinanceDoesNotPublishFailedCommit(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable r", Columns: 6}, scriptdb.Step{Kind: "query", SQL: "FROM settlement_receipt rc", Columns: 3}, scriptdb.Step{Kind: "commit", Err: errors.New("commit failure")})
	got, err := (&Service{DB: db}).DashboardFinance(context.Background(), "tenant", time.Now())
	if err == nil || got.Available {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

// TestDashboardFinanceInvalidSourceDoesNotBecomeZero 验证源异常不得伪装成功零值。
// 参数：t为测试上下文。
// 返回值：无，依赖异常、金额溢出或取消后发布成功时失败。
func TestDashboardFinanceInvalidSourceDoesNotBecomeZero(t *testing.T) {
	for _, tc := range []struct {
		name, amount string
		queryErr     error
	}{
		{"overflow", "92233720368547758.08", nil}, {"precision", "0.001", nil}, {"source failure", "", errors.New("source unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable r", Rows: [][]driver.Value{{"CNY", tc.amount, "0.00", "0.00", "0.00", nil}}, Err: tc.queryErr}, scriptdb.Step{Kind: "rollback"})
			got, err := (&Service{DB: db}).DashboardFinance(context.Background(), "tenant", time.Now())
			if err == nil || got.Available {
				t.Fatalf("snapshot=%+v err=%v", got, err)
			}
		})
	}
	db := scriptdb.Open(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Service{DB: db}).DashboardFinance(ctx, "tenant", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err=%v", err)
	}
}

// TestDashboardFinanceCanceledAtCommitDoesNotPublish 验证取消发生在提交边界时不发布。
// 参数：t为测试上下文。
// 返回值：无，提交瞬间取消仍被返回为可用快照时失败。
func TestDashboardFinanceCanceledAtCommitDoesNotPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable r", Columns: 6}, scriptdb.Step{Kind: "query", SQL: "FROM settlement_receipt rc", Columns: 3}, scriptdb.Step{Kind: "commit", After: cancel})
	got, err := (&Service{DB: db}).DashboardFinance(ctx, "tenant", time.Now())
	if !errors.Is(err, context.Canceled) || got.Available {
		t.Fatalf("snapshot=%+v err=%v", got, err)
	}
}
