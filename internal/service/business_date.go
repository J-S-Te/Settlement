package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/j-s-te/settlement/internal/businessclock"
)

// businessDateLayout 是业务日期（回款日/开票日/计划日）的文本布局。
const businessDateLayout = "2006-01-02"

// validateBusinessDateOn 以给定北京业务日判定业务日期是否合法。
// 参数：value 为 YYYY-MM-DD 文本；field 为错误文案里的字段名；
// today 为北京业务日（00:00，Location=UTC 的日历日，来自 businessclock.Today）。
// 返回值：归一化日期；格式错误或晚于业务日返回 ErrInvalid。
// 该内核不读时钟，便于单测覆盖北京日边界（UTC 仍是前一日、北京已是次日）。
// 比较两端都是“北京业务日”编码，按自然日比较，不能拿 UTC 当前时间戳直接比日期串。
func validateBusinessDateOn(value, field string, today time.Time) (string, error) {
	normalized := strings.TrimSpace(value)
	day, err := time.Parse(businessDateLayout, normalized)
	if err != nil {
		return "", fmt.Errorf("%w: %s must be YYYY-MM-DD", ErrInvalid, field)
	}
	if day.After(today) {
		return "", fmt.Errorf("%w: %s must not be later than the Beijing business day", ErrInvalid, field)
	}
	return normalized, nil
}

// validateBusinessDate 用当前北京业务日校验回款日/开票日/计划日等业务日期。
// 参数：value 为 YYYY-MM-DD 文本；field 为错误文案里的字段名。
// 返回值：归一化日期或 ErrInvalid。
func validateBusinessDate(value, field string) (string, error) {
	return validateBusinessDateOn(value, field, businessclock.Today())
}
