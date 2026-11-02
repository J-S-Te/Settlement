package businessclock

import (
	"testing"
	"time"
)

// driverWallClock 复刻 mysql 驱动在 DSN loc=UTC 下把一个 time.Time 参数
// 渲染成 SQL 字面量时的墙钟行为，用于断言绑定参数不偏移。
// 参数：t 为待绑定参数；返回值：驱动实际写入 SQL 的墙钟字符串（秒级）。
func driverWallClock(t time.Time) string {
	return t.In(time.UTC).Format("2006-01-02 15:04:05")
}

// withNow 用固定时刻替换包级时钟，测试结束后恢复。
func withNow(t *testing.T, instant time.Time) {
	t.Helper()
	previous := nowFunc
	nowFunc = func() time.Time { return instant }
	t.Cleanup(func() { nowFunc = previous })
}

// TestLocationIsUTCPlus8 验证业务时区稳定为 UTC+8。
func TestLocationIsUTCPlus8(t *testing.T) {
	if _, offset := time.Now().In(Location()).Zone(); offset != 8*3600 {
		t.Fatalf("business offset=%d", offset)
	}
}

// TestTodayUsesBeijingCalendarDayWithUTCWallClock 验证 Today 返回北京日历日，
// 但 Location 必须是 time.UTC、时间必须是 00:00:00，绑定到 SQL 后是
// “<北京今天> 00:00:00”而不是“前一天 16:00:00”。
func TestTodayUsesBeijingCalendarDayWithUTCWallClock(t *testing.T) {
	// 2025-12-31T16:30Z 已是北京 2026-01-01 00:30。
	withNow(t, time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC))
	today := Today()
	if today.Location() != time.UTC {
		t.Fatalf("Today location=%v; want time.UTC", today.Location())
	}
	if got := today.Format("2006-01-02 15:04:05"); got != "2026-01-01 00:00:00" {
		t.Fatalf("Today wall clock=%s", got)
	}
	if got := driverWallClock(today); got != "2026-01-01 00:00:00" {
		t.Fatalf("bound sql wall clock=%s", got)
	}
	if got := driverWallClock(today); got == "2025-12-31 16:00:00" {
		t.Fatalf("bound sql wall clock fell back to previous-day 16:00: %s", got)
	}
}

// TestTodayBeforeAndAfterBeijingMidnight 覆盖北京 00:00 前后的日历日翻转：
// 跨年（2025-12-31→2026-01-01）与同一 UTC 日内的普通翻转。
func TestTodayBeforeAndAfterBeijingMidnight(t *testing.T) {
	cases := []struct {
		name    string
		instant time.Time
		want    string
	}{
		{"北京跨年前最后时刻", time.Date(2025, 12, 31, 15, 59, 59, 0, time.UTC), "2025-12-31"},
		{"北京跨年后第一时刻", time.Date(2025, 12, 31, 16, 0, 0, 0, time.UTC), "2026-01-01"},
		{"北京跨年后半小时", time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC), "2026-01-01"},
		{"北京跨月前最后时刻", time.Date(2026, 1, 31, 15, 59, 59, 0, time.UTC), "2026-01-31"},
		{"北京跨月后第一时刻", time.Date(2026, 1, 31, 16, 0, 0, 0, time.UTC), "2026-02-01"},
		{"闰日", time.Date(2024, 2, 29, 16, 0, 0, 0, time.UTC), "2024-03-01"},
	}
	for _, c := range cases {
		withNow(t, c.instant)
		if got := Today().Format("2006-01-02"); got != c.want {
			t.Fatalf("%s: Today=%s; want %s", c.name, got, c.want)
		}
	}
}

// TestMonthBoundaries 验证本月首末日按北京日历日计算并沿用 UTC 墙钟约定。
func TestMonthBoundaries(t *testing.T) {
	// 北京 2026-01-01 00:30，UTC 仍是 2025-12-31。
	withNow(t, time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC))
	if got := MonthStart().Format("2006-01-02 15:04:05"); got != "2026-01-01 00:00:00" {
		t.Fatalf("MonthStart=%s", got)
	}
	if got := MonthEnd().Format("2006-01-02 15:04:05"); got != "2026-01-31 00:00:00" {
		t.Fatalf("MonthEnd=%s", got)
	}
	if MonthStart().Location() != time.UTC || MonthEnd().Location() != time.UTC {
		t.Fatalf("month bounds location=%v/%v", MonthStart().Location(), MonthEnd().Location())
	}
	// 北京 2026-02-01 之前的最后时刻仍是 1 月。
	withNow(t, time.Date(2026, 1, 31, 15, 59, 59, 0, time.UTC))
	if got := MonthEnd().Format("2006-01-02"); got != "2026-01-31" {
		t.Fatalf("MonthEnd for january=%s", got)
	}
	// 二月（平年）与闰年二月的月末。
	withNow(t, time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC))
	if got := MonthEnd().Format("2006-01-02"); got != "2026-02-28" {
		t.Fatalf("MonthEnd for 2026-02=%s", got)
	}
	withNow(t, time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC))
	if got := MonthEnd().Format("2006-01-02"); got != "2024-02-29" {
		t.Fatalf("MonthEnd for 2024-02=%s", got)
	}
}

// TestDateStringUsesBeijingDay 验证 DateString 按北京日历日格式化。
func TestDateStringUsesBeijingDay(t *testing.T) {
	if got := DateString(time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC)); got != "2026-01-01" {
		t.Fatalf("DateString=%s", got)
	}
	if got := DateString(time.Date(2025, 12, 31, 15, 30, 0, 0, time.UTC)); got != "2025-12-31" {
		t.Fatalf("DateString=%s", got)
	}
	withNow(t, time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC))
	if got := DateString(Now()); got != Today().Format("2006-01-02") {
		t.Fatalf("DateString(Now())=%s; Today=%s", got, Today().Format("2006-01-02"))
	}
}
