package service

import (
	"fmt"
	"strings"
)

// BuildAgingCase 用途：为报表和导出生成同一套经过验证的五档账龄表达式。
// 参数：days为四个递增边界，column为代码内固定到期日期列。
// 返回值：返回SQL CASE表达式；边界或列不可信时返回参数错误。
func BuildAgingCase(days []int, column string) (string, error) {
	if len(days) != 4 || (column != "due_date" && column != "r.due_date") {
		return "", ErrInvalid
	}
	parts := []string{"CASE"}
	lower := 0
	for _, upper := range days {
		if upper < lower || upper > 36500 {
			return "", ErrInvalid
		}
		parts = append(parts, fmt.Sprintf("WHEN DATEDIFF(UTC_DATE(),%s)<=%d THEN '%d-%d'", column, upper, lower, upper))
		lower = upper + 1
	}
	parts = append(parts, fmt.Sprintf("ELSE '%d+' END", days[3]))
	return strings.Join(parts, " "), nil
}
