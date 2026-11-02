// Package businessclock 提供按北京时间（UTC+8）计算的业务日历值。
//
// 关键约定（改代码前必须先读）：
//   - 数据源 DSN 固定 loc=UTC，MySQL 也没有 --default-time-zone，因此
//     DATE 列和 DATETIME 列存的都是 UTC 墙钟；数据库里的 CURDATE()/UTC_DATE()
//     取到的是 UTC 当日，而业务要求的“今天”是北京当日。两者在北京时间
//     00:00–08:00 会相差一天，所以 SQL 里绝不能再出现 CURDATE()/UTC_DATE()。
//   - Today/MonthStart/MonthEnd 返回的是**北京日历日**，但返回值的时间部分
//     是 00:00:00，且 Location 必须是 time.UTC。这样 mysql 驱动按 loc=UTC
//     把它格式化成 "<北京日期> 00:00:00" 的墙钟字符串绑定进 SQL：与 DATE 列
//     按自然日比较不会偏移，与 UTC 墙钟 DATETIME 列比较也不会偏移 8 小时。
//   - 反例：若返回 time.Now().In(Location()) 这类北京墙钟值，驱动会输出
//     "<北京日期+1> 16:00:00" 之前一天的 16:00:00，`due_date<today` 就会
//     整体推迟 8 小时，出现“北京已经到期却仍算未到期”的错误。
//   - 存储时间戳（created_at/updated_at/sent_at 等）仍然用 UTC_TIMESTAMP(3)
//     或 time.Now().UTC()，不要用本包的 Today()/Now() 覆盖它们。
package businessclock

import (
	"sync"
	"time"
)

// dateLayout 是 DATE 列/业务日历日的字符串形式。
const dateLayout = "2006-01-02"

// businessZone 是业务时区；Asia/Shanghai 缺失（如精简镜像无 tzdata）时回落到
// 固定 UTC+8，两者对日历日的判定完全等价。
var businessZone = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
})

// nowFunc 允许测试注入确定的当前时刻。生产路径始终是 time.Now。
var nowFunc = time.Now

// Location 返回业务时区（北京时间 UTC+8）。
// 返回值：Asia/Shanghai，或 UTC+8 固定时区。
func Location() *time.Location {
	return businessZone()
}

// Now 返回北京时区的当前时刻（Instant 不变，只是墙钟按北京展示）。
// 只用于“今天是什么日子”和展示；不要把它绑定到 DATETIME 列，
// 否则会与 UTC 墙钟存储相差 8 小时。
func Now() time.Time {
	return nowFunc().In(Location())
}

// Today 返回北京“今天”的 00:00:00，Location 固定为 time.UTC。
// 返回值：可直接作为 SQL 参数绑定到 DATE 比较/DATEDIFF 的业务日。
func Today() time.Time {
	return todayAt(nowFunc())
}

// MonthStart 返回北京“本月”1 日 00:00:00，Location 固定为 time.UTC。
func MonthStart() time.Time {
	return monthStartAt(nowFunc())
}

// MonthEnd 返回北京“本月”最后一天 00:00:00，Location 固定为 time.UTC。
// 它与 DATE 列比较时等价于“包含当月最后一天”。
func MonthEnd() time.Time {
	return monthEndAt(nowFunc())
}

// DateString 把时刻格式化为北京时区的 YYYY-MM-DD。
// 参数：t 为任意时区的时间；返回值：北京日历日字符串。
func DateString(t time.Time) string {
	return t.In(Location()).Format(dateLayout)
}

// todayAt 是 Today 的纯函数内核，便于按固定时刻断言日历边界。
// 参数：now 为当前时刻；返回值：北京当日 00:00，Location 为 time.UTC。
func todayAt(now time.Time) time.Time {
	return calendarDay(now, Location())
}

// monthStartAt 是 MonthStart 的纯函数内核。
func monthStartAt(now time.Time) time.Time {
	year, month, _ := now.In(Location()).Date()
	return time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
}

// monthEndAt 是 MonthEnd 的纯函数内核。
func monthEndAt(now time.Time) time.Time {
	return monthStartAt(now).AddDate(0, 1, -1)
}

// calendarDay 把 loc 时区下的日历日压成 00:00:00 且 Location 为 time.UTC 的值。
func calendarDay(now time.Time, loc *time.Location) time.Time {
	year, month, day := now.In(loc).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
