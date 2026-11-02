package httpapi

import (
	"database/sql/driver"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/businessclock"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestDashboardBindsBeijingBusinessDay 截获看板聚合的绑定参数，断言逾期判定用
// 北京业务日、本月回款窗口用北京业务月首末日（而不是数据库 UTC 当日）。
// 参数：t 测试上下文。
// 返回值：无，绑定值或占位符顺序不符合约定即失败。
func TestDashboardBindsBeijingBusinessDay(t *testing.T) {
	var overdueArgs []driver.NamedValue
	var overdueSQL string
	var receiptArgs []driver.NamedValue
	var receiptSQL string
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "query", SQL: "CASE WHEN due_date<? AND open_amount>0 THEN 1", Args: &overdueArgs, SQLOut: &overdueSQL, Rows: [][]driver.Value{{int64(0), int64(0)}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan WHERE tenant_id=?", Rows: [][]driver.Value{{int64(0)}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_request WHERE tenant_id=?", Rows: [][]driver.Value{{int64(0)}}},
		scriptdb.Step{Kind: "query", SQL: "COALESCE(SUM(open_amount),0) FROM settlement_receivable WHERE tenant_id=?", Rows: [][]driver.Value{}},
		scriptdb.Step{Kind: "query", SQL: "COALESCE(SUM(amount),0) FROM settlement_receipt", Args: &receiptArgs, SQLOut: &receiptSQL, Rows: [][]driver.Value{}},
		scriptdb.Step{Kind: "query", SQL: "ir.amount_incl_tax", Rows: [][]driver.Value{}},
		scriptdb.Step{Kind: "query", SQL: "AND due_date<? GROUP BY currency", Rows: [][]driver.Value{}},
	)
	handler := New(db, config.Config{DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/dashboard", nil))
	if response.Code != 200 {
		t.Fatalf("HTTP=%d body=%s", response.Code, response.Body.String())
	}

	today := businessclock.Today()
	assertDashboardBusinessDay(t, "看板逾期判定", overdueArgs[0].Value, today)
	if got := overdueArgs[1].Value; got != "dev" {
		t.Fatalf("看板逾期判定租户=%v; want dev", got)
	}
	if got, want := strings.Count(overdueSQL, "?"), len(overdueArgs); got != want {
		t.Fatalf("看板逾期查询占位符=%d 参数=%d", got, want)
	}

	monthStart := businessclock.MonthStart()
	assertDashboardBusinessDay(t, "本月回款窗口起点", receiptArgs[1].Value, monthStart)
	assertDashboardBusinessDay(t, "本月回款窗口终点", receiptArgs[2].Value, monthStart.AddDate(0, 1, -1))
	if got := receiptArgs[0].Value; got != "dev" {
		t.Fatalf("本月回款租户=%v; want dev", got)
	}
	if got, want := strings.Count(receiptSQL, "?"), len(receiptArgs); got != want {
		t.Fatalf("本月回款查询占位符=%d 参数=%d", got, want)
	}
}

// assertDashboardBusinessDay 断言绑定值是 UTC location 的 00:00，并把
// 北京日历日（而不是前一天 16:00）写进 SQL。
// 参数：t 测试上下文，name 断言名称，value 实际绑定值，want 期望业务日。
// 返回值：无，不符合约定即失败。
func assertDashboardBusinessDay(t *testing.T, name string, value any, want time.Time) {
	t.Helper()
	stamp, ok := value.(time.Time)
	if !ok {
		t.Fatalf("%s: 绑定参数类型=%T; want time.Time", name, value)
	}
	if stamp.Location() != time.UTC {
		t.Fatalf("%s: Location=%v; want time.UTC", name, stamp.Location())
	}
	if !stamp.Equal(want) {
		t.Fatalf("%s: 绑定值=%v; want %v", name, stamp, want)
	}
	if got := stamp.In(time.UTC).Format("2006-01-02 15:04:05"); !strings.HasSuffix(got, " 00:00:00") {
		t.Fatalf("%s: 绑定墙钟=%s; want 00:00:00", name, got)
	}
}
