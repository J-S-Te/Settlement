package service

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/businessclock"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestDunningOverviewBindsBeijingBusinessDay 截获催收总览聚合查询的绑定参数，
// 断言逾期判定与账龄判定用的都是北京业务日，且占位符数量与参数数量一致
// （参数错位会让 MySQL 在运行期报错或把租户绑成日期）。
// 参数：t 测试上下文。
// 返回值：无，绑定值或位置不符合约定即失败。
func TestDunningOverviewBindsBeijingBusinessDay(t *testing.T) {
	var overviewArgs []driver.NamedValue
	var overviewSQL string
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "query", SQL: "r.due_date<?", Args: &overviewArgs, SQLOut: &overviewSQL, Rows: [][]driver.Value{{int64(0), "0.00", int64(0), int64(0), int64(0), int64(0), int64(0)}}},
		scriptdb.Step{Kind: "query", SQL: "AND due_date<?", Rows: [][]driver.Value{}},
	)
	overview, err := (&Service{DB: db}).DunningOverview(context.Background(), Principal{TenantID: "tenant-bd"})
	if err != nil {
		t.Fatal(err)
	}
	if overview.OverdueCount != 0 || overview.CurrencyTotals == nil {
		t.Fatalf("overview=%+v", overview)
	}
	if got, want := strings.Count(overviewSQL, "?"), len(overviewArgs); got != want {
		t.Fatalf("占位符=%d 参数=%d", got, want)
	}
	today := businessclock.Today()
	// 顺序：两处逾期判定 → 四个案件/动作子查询租户 → 未覆盖逾期（租户、逾期、
	// 账龄）→ 外层租户。
	for _, index := range []int{0, 1, 7, 8} {
		if got := overviewArgs[index].Value; got != any(today) {
			t.Fatalf("第%d个参数=%v; want 北京业务日 %v", index, got, today)
		}
	}
	for _, index := range []int{2, 3, 4, 5, 6, 9} {
		if got := overviewArgs[index].Value; got != any("tenant-bd") {
			t.Fatalf("第%d个参数=%v; want 租户", index, got)
		}
	}
}

// TestAgingSummaryBindsBeijingBusinessDay 截获账龄汇总查询的绑定参数，断言
// 五档分档与 ORDER BY 账龄基准都是北京业务日，且占位符与参数数量一致。
// 参数：t 测试上下文。
// 返回值：无，绑定值或顺序不符合约定即失败。
func TestAgingSummaryBindsBeijingBusinessDay(t *testing.T) {
	var args []driver.NamedValue
	var query string
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "query", SQL: "SELECT settings_json,version FROM settlement_settings", Rows: [][]driver.Value{}},
		scriptdb.Step{Kind: "query", SQL: "GROUP BY currency,bucket", Args: &args, SQLOut: &query, Rows: [][]driver.Value{}},
	)
	items, err := (&Service{DB: db}).AgingSummary(context.Background(), Principal{TenantID: "tenant-bd"})
	if err != nil {
		t.Fatal(err)
	}
	if items == nil {
		t.Fatal("aging summary 返回 nil 切片")
	}
	if got, want := strings.Count(query, "?"), len(args); got != want {
		t.Fatalf("占位符=%d 参数=%d", got, want)
	}
	if len(args) != 6 {
		t.Fatalf("账龄汇总绑定参数=%d; want 6", len(args))
	}
	today := businessclock.Today()
	// 顺序：CASE 四个分档 → WHERE 租户 → ORDER BY 账龄基准。
	for index := 0; index < 4; index++ {
		if got := args[index].Value; got != any(today) {
			t.Fatalf("第%d个参数=%v; want 北京业务日 %v", index, got, today)
		}
	}
	if got := args[4].Value; got != any("tenant-bd") {
		t.Fatalf("租户参数=%v", got)
	}
	if got := args[5].Value; got != any(today) {
		t.Fatalf("账龄排序参数=%v; want %v", got, today)
	}
}
