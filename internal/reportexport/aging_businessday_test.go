package reportexport

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/businessclock"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestAgingCSVBindsBeijingBusinessDay 截获账龄导出的绑定参数，断言逾期天数与
// 五档账龄都以北京业务日为基准，且占位符数量与参数数量一致（参数错位会把
// 日期绑到租户位置或反之）。
// 参数：t 测试上下文。
// 返回值：无，绑定值或位置不符合约定即失败。
func TestAgingCSVBindsBeijingBusinessDay(t *testing.T) {
	var args []driver.NamedValue
	var query string
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "query", SQL: "SELECT settings_json,version FROM settlement_settings", Rows: [][]driver.Value{}},
		scriptdb.Step{Kind: "query", SQL: "GREATEST(TIMESTAMPDIFF(DAY,r.due_date,?),0)", Args: &args, SQLOut: &query, Rows: [][]driver.Value{}},
	)
	if _, err := (&Worker{DB: db}).agingCSV(context.Background(), "tenant-bd"); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Count(query, "?"), len(args); got != want {
		t.Fatalf("占位符=%d 参数=%d", got, want)
	}
	if len(args) != 6 {
		t.Fatalf("账龄导出绑定参数=%d; want 6", len(args))
	}
	// 顺序：SELECT 中的逾期天数基准日 → 账龄 CASE 四个分档 → WHERE 租户。
	for index := 0; index < 5; index++ {
		stamp, ok := args[index].Value.(time.Time)
		if !ok {
			t.Fatalf("第%d个参数类型=%T; want time.Time", index, args[index].Value)
		}
		if stamp.Location() != time.UTC || stamp.Hour() != 0 || stamp.Minute() != 0 {
			t.Fatalf("第%d个参数=%v; want time.UTC 的 00:00", index, stamp)
		}
		if !stamp.Equal(businessclock.Today()) {
			t.Fatalf("第%d个参数=%v; want 北京业务日 %v", index, stamp, businessclock.Today())
		}
	}
	if got := args[5].Value; got != "tenant-bd" {
		t.Fatalf("租户参数=%v", got)
	}
}
