package service

import (
	"strings"
	"testing"
)

// TestAgingRules 用途：验证五档区间和SQL标识符白名单。
// 参数：t为测试上下文。
// 返回值：无，错误区间或可注入标识符未拒绝时失败。
func TestAgingRules(t *testing.T) {
	q, err := BuildAgingCase([]int{30, 60, 90, 180}, "r.due_date")
	if err != nil || !strings.Contains(q, "'91-180'") || !strings.Contains(q, "'180+'") {
		t.Fatalf("%s %v", q, err)
	}
	if _, err = BuildAgingCase([]int{30, 20, 90, 180}, "r.due_date"); err == nil {
		t.Fatal("乱序未拒绝")
	}
	if _, err = BuildAgingCase([]int{30, 60, 90, 180}, "due_date);DROP TABLE x;"); err == nil {
		t.Fatal("非法列未拒绝")
	}
}
