package service

import (
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/businessclock"
)

// TestAgingRules 用途：验证五档区间和SQL标识符白名单。
// 参数：t为测试上下文。
// 返回值：无，错误区间或可注入标识符未拒绝时失败。
func TestAgingRules(t *testing.T) {
	q, args, err := BuildAgingCase([]int{30, 60, 90, 180}, "r.due_date")
	if err != nil || !strings.Contains(q, "'91-180'") || !strings.Contains(q, "'180+'") {
		t.Fatalf("%s %v", q, err)
	}
	if _, _, err = BuildAgingCase([]int{30, 20, 90, 180}, "r.due_date"); err == nil {
		t.Fatal("乱序未拒绝")
	}
	if _, _, err = BuildAgingCase([]int{30, 60, 90, 180}, "due_date);DROP TABLE x;"); err == nil {
		t.Fatal("非法列未拒绝")
	}
	// 账龄基准日必须是绑定参数（北京业务日），不能再依赖数据库会话时区的
	// CURDATE()/UTC_DATE()。
	if strings.Contains(q, "UTC_DATE()") || strings.Contains(q, "CURDATE()") {
		t.Fatalf("账龄表达式仍然内联数据库时钟: %s", q)
	}
	if got := strings.Count(q, "?"); got != len(args) || got != 4 {
		t.Fatalf("占位符=%d 参数=%d; want 4", got, len(args))
	}
	today := businessclock.Today()
	for index, arg := range args {
		if arg != today {
			t.Fatalf("第%d个账龄参数=%v; want %v", index, arg, today)
		}
	}
}
