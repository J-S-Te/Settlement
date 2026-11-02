package service

import (
	"fmt"
	"strings"

	"github.com/j-s-te/settlement/internal/businessclock"
)

// BuildAgingCase 用途：为报表和导出生成同一套经过验证的五档账龄表达式。
// 参数：days为四个递增边界，column为代码内固定到期日期列。
// 返回值：返回SQL CASE表达式、与表达式中占位符一一对应的绑定参数
// （每个分档一个北京业务日）、以及参数错误。
//
// 账龄基准日必须是调用方绑定的北京业务日（businessclock.Today()），
// 不能再用 CURDATE()/UTC_DATE()：数据源 loc=UTC 且 MySQL 会话时区为 UTC，
// 数据库当日与北京当日在北京时间 00:00–08:00 相差一天。
// 调用方必须按占位符出现顺序把 args 绑定到整条 SQL。
func BuildAgingCase(days []int, column string) (string, []any, error) {
	if len(days) != 4 || (column != "due_date" && column != "r.due_date") {
		return "", nil, ErrInvalid
	}
	parts := []string{"CASE"}
	args := make([]any, 0, len(days))
	today := businessclock.Today()
	lower := 0
	for _, upper := range days {
		if upper < lower || upper > 36500 {
			return "", nil, ErrInvalid
		}
		parts = append(parts, fmt.Sprintf("WHEN DATEDIFF(?,%s)<=%d THEN '%d-%d'", column, upper, lower, upper))
		args = append(args, today)
		lower = upper + 1
	}
	parts = append(parts, fmt.Sprintf("ELSE '%d+' END", days[3]))
	return strings.Join(parts, " "), args, nil
}
