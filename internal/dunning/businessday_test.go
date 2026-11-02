package dunning

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/businessclock"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// driverWallClock 复刻 mysql 驱动在 DSN loc=UTC 下把 time.Time 参数渲染成
// SQL 字面量时的墙钟（秒级），用于断言绑定参数不整体偏移 8 小时。
// 参数：stamp 为待绑定参数。
// 返回值：驱动实际写入 SQL 的墙钟字符串。
func driverWallClock(stamp time.Time) string {
	return stamp.In(time.UTC).Format("2006-01-02 15:04:05")
}

// agingDays 是 SQL 侧 DATEDIFF(<北京业务日>,due_date) 的 Go 镜像，
// 只用于断言业务边界，不参与生产查询。
// 参数：today 为北京业务日（UTC location 的 00:00），due 为到期日。
// 返回值：逾期天数（到期日当天为 0，次日起为正）。
func agingDays(today, due time.Time) int {
	return int(today.Sub(due) / (24 * time.Hour))
}

// assertBeijingBusinessDay 断言绑定参数是北京业务日墙钟：Location 为 time.UTC、
// 时间部分是 00:00:00，驱动渲染出的日期等于北京当天而不是前一天 16:00。
// 参数：t 测试上下文，name 断言名称，value 为实际绑定值。
// 返回值：无，不符合约定即失败。
func assertBeijingBusinessDay(t *testing.T, name string, value any) {
	t.Helper()
	stamp, ok := value.(time.Time)
	if !ok {
		t.Fatalf("%s: 绑定参数类型=%T; want time.Time", name, value)
	}
	if stamp.Location() != time.UTC {
		t.Fatalf("%s: Location=%v; want time.UTC（否则驱动会写北京墙钟，偏移 8 小时）", name, stamp.Location())
	}
	if hour, minute, second := stamp.Clock(); hour != 0 || minute != 0 || second != 0 || stamp.Nanosecond() != 0 {
		t.Fatalf("%s: 时间部分=%s; want 00:00:00", name, stamp.Format("15:04:05.999999999"))
	}
	want := businessclock.DateString(stamp) + " 00:00:00"
	if got := driverWallClock(stamp); got != want {
		t.Fatalf("%s: 绑定墙钟=%s; want %s（出现在前一天 16:00 说明用了北京墙钟）", name, got, want)
	}
}

// TestOverdueDaysAndPhaseBoundaryAtBusinessDay 覆盖“到期日当天 vs 次日”的业务
// 边界：逾期天数、逾期判定（SQL 的 r.due_date<?）和提醒 phase 必须在同一个
// 北京业务日上翻转，不能在北京时间 00:00–08:00 提前或延后一天。
// 参数：t 测试上下文。
// 返回值：无，任一边界不齐或绑定值偏移即失败。
func TestOverdueDaysAndPhaseBoundaryAtBusinessDay(t *testing.T) {
	rules := service.ReminderRules{LeadDays: 3, RepeatDays: 7, EscalationDays: 30}
	today := businessclock.Today()
	for _, c := range []struct {
		name        string
		due         time.Time
		wantAging   int
		wantOverdue bool
		wantPhase   string
	}{
		{"到期日当天（北京今天）不算逾期", today, 0, false, "DUE_SOON"},
		{"到期日次日（北京昨天）逾期一天", today.AddDate(0, 0, -1), 1, true, "OVERDUE"},
		{"升级阈值当天", today.AddDate(0, 0, -rules.EscalationDays), rules.EscalationDays, true, "ESCALATED"},
		{"升级阈值前一天", today.AddDate(0, 0, -(rules.EscalationDays - 1)), rules.EscalationDays - 1, true, "OVERDUE"},
		{"提醒窗口首日（提前 LeadDays 天）", today.AddDate(0, 0, rules.LeadDays), -rules.LeadDays, false, "DUE_SOON"},
		{"提醒窗口前一天不提醒", today.AddDate(0, 0, rules.LeadDays+1), -rules.LeadDays - 1, false, ""},
	} {
		if got := agingDays(today, c.due); got != c.wantAging {
			t.Fatalf("%s: 逾期天数=%d; want %d", c.name, got, c.wantAging)
		}
		// 立案/逾期门槛在 SQL 里是 r.due_date<?，绑定北京业务日后等价于 due<today。
		if got := c.due.Before(today); got != c.wantOverdue {
			t.Fatalf("%s: 逾期判定=%v; want %v", c.name, got, c.wantOverdue)
		}
		if got := reminderPhase(agingDays(today, c.due), false, rules); got != c.wantPhase {
			t.Fatalf("%s: phase=%q; want %q", c.name, got, c.wantPhase)
		}
	}
	assertBeijingBusinessDay(t, "Today()", today)
}

// TestReminderPhaseFlipsOnlyAtBeijingMidnight 验证同一到期日在北京 00:00 前后
// 的 phase 只能翻转一次：北京 23:59 仍是 DUE_SOON（到期日当天），北京 00:00
// 之后才是 OVERDUE。这里用 Today 的日历日推进来复现跨日，而不是用 UTC 日期。
// 参数：t 测试上下文。
// 返回值：无，phase 在到期日当天就升级即失败。
func TestReminderPhaseFlipsOnlyAtBeijingMidnight(t *testing.T) {
	rules := service.ReminderRules{LeadDays: 3, RepeatDays: 7, EscalationDays: 30}
	due := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 北京 2026-01-01 23:59（UTC 仍是 2026-01-01 15:59）：业务日仍是 1 月 1 日。
	beforeMidnight := todayFromInstant(t, time.Date(2026, 1, 1, 15, 59, 0, 0, time.UTC))
	// 北京 2026-01-02 00:00（UTC 2026-01-01 16:00）：业务日推进到 1 月 2 日。
	afterMidnight := todayFromInstant(t, time.Date(2026, 1, 1, 16, 0, 0, 0, time.UTC))
	if got := agingDays(beforeMidnight, due); got != 0 {
		t.Fatalf("北京 00:00 前逾期天数=%d; want 0", got)
	}
	if got := reminderPhase(agingDays(beforeMidnight, due), false, rules); got != "DUE_SOON" {
		t.Fatalf("北京 00:00 前 phase=%q; want DUE_SOON", got)
	}
	if got := agingDays(afterMidnight, due); got != 1 {
		t.Fatalf("北京 00:00 后逾期天数=%d; want 1", got)
	}
	if got := reminderPhase(agingDays(afterMidnight, due), false, rules); got != "OVERDUE" {
		t.Fatalf("北京 00:00 后 phase=%q; want OVERDUE", got)
	}
	// 案门槛同样只在跨过北京 00:00 后翻转为逾期。
	if due.Before(beforeMidnight) {
		t.Fatal("到期日当天不该立案")
	}
	if !due.Before(afterMidnight) {
		t.Fatal("北京次日应判为逾期")
	}
}

// todayFromInstant 按北京日历日把固定时刻折算成业务日（UTC location 的 00:00），
// 与 businessclock.Today 的约定保持一致，供边界测试构造跨日时刻。
// 参数：t 测试上下文，instant 为任意时区的时刻。
// 返回值：该时刻对应的北京业务日。
func todayFromInstant(t *testing.T, instant time.Time) time.Time {
	t.Helper()
	beijing := instant.In(businessclock.Location())
	return time.Date(beijing.Year(), beijing.Month(), beijing.Day(), 0, 0, 0, 0, time.UTC)
}

// TestScannerBindsBeijingBusinessDayToSQL 用边界驱动截获真实查询的绑定参数，
// 断言逾期天数/立案门槛传入的是北京业务日墙钟（<北京今天> 00:00:00），
// 而不是北京墙钟值渲染出的“前一天 16:00:00”。
// 参数：t 测试上下文。
// 返回值：无，绑定值不符合约定即失败。
func TestScannerBindsBeijingBusinessDayToSQL(t *testing.T) {
	var reopenArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "exec", SQL: "r.due_date<?", Args: &reopenArgs},
	)
	scanner := &Scanner{DB: db}
	if err := scanner.reopenSettled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reopenArgs) != 2 {
		t.Fatalf("reopenSettled 绑定参数=%d; want 2", len(reopenArgs))
	}
	for index, arg := range reopenArgs {
		assertBeijingBusinessDay(t, "reopenSettled参数", arg.Value)
		if !arg.Value.(time.Time).Equal(businessclock.Today()) {
			t.Fatalf("reopenSettled 第%d个参数不是 Today(): %v", index, arg.Value)
		}
	}

	// 候选查询只返回零行：绑定参数在驱动层已经截获，断言不必依赖下游
	// 逐步演进的列/事务步骤。
	var candidateArgs []driver.NamedValue
	db = scriptdb.Open(t,
		scriptdb.Step{Kind: "query", SQL: "DATEDIFF(?,r.due_date)", Args: &candidateArgs, Rows: [][]driver.Value{}},
	)
	scanner = &Scanner{DB: db, BatchSize: 50, Directory: collectorStub{eligible: true}}
	if err := scanner.createInitialCases(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 候选查询绑定：逾期天数、账龄区间、逾期门槛、批量上限。
	if len(candidateArgs) != 4 {
		t.Fatalf("候选查询绑定参数=%d; want 4", len(candidateArgs))
	}
	for index := 0; index < 3; index++ {
		assertBeijingBusinessDay(t, "立案门槛参数", candidateArgs[index].Value)
		if !candidateArgs[index].Value.(time.Time).Equal(businessclock.Today()) {
			t.Fatalf("候选查询第%d个参数不是 Today(): %v", index, candidateArgs[index].Value)
		}
	}
	if got := candidateArgs[3].Value; got != int64(50) {
		t.Fatalf("批量上限=%v; want 50", got)
	}
}

// TestRoleReminderBindsBeijingBusinessDay 截获负责人提醒候选查询的绑定参数，
// 断言按实际 SQL 顺序将租户、提醒窗口与阶段 CASE 绑定到正确业务参数。
// 参数：t 测试上下文。
// 返回值：无，绑定值或位置不符合约定即失败。
func TestRoleReminderBindsBeijingBusinessDay(t *testing.T) {
	var reminderArgs []driver.NamedValue
	var reminderSQL string
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "query", SQL: "SELECT settings_json,version FROM settlement_settings", Rows: [][]driver.Value{}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_dunning_action target"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_dunning_case target"},
		scriptdb.Step{Kind: "query", SQL: "DATEDIFF(?,rp.due_date)", Args: &reminderArgs, SQLOut: &reminderSQL, Rows: [][]driver.Value{}},
	)
	worker := &RoleReminderWorker{DB: db, Service: &service.Service{DB: db}, TenantID: "tenant-bd", Directory: &roleDirectoryFixture{}}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Count(reminderSQL, "?"), len(reminderArgs); got != want {
		t.Fatalf("占位符=%d 参数=%d", got, want)
	}
	if len(reminderArgs) != 8 {
		t.Fatalf("提醒候选绑定参数=%d; want 8", len(reminderArgs))
	}
	// WHERE 先限定租户，随后 DATE_ADD(?, INTERVAL ? DAY) 才接收业务日与天数。
	for _, index := range []int{0, 1, 3, 5} {
		assertBeijingBusinessDay(t, "提醒候选参数", reminderArgs[index].Value)
		if !reminderArgs[index].Value.(time.Time).Equal(businessclock.Today()) {
			t.Fatalf("第%d个参数不是 Today(): %v", index, reminderArgs[index].Value)
		}
	}
	if got := reminderArgs[2].Value; got != int64(30) {
		t.Fatalf("升级阈值=%v; want 30", got)
	}
	if got := reminderArgs[6].Value; got != int64(3) {
		t.Fatalf("提前天数=%v; want 3", got)
	}
	if got := reminderArgs[4].Value; got != "tenant-bd" {
		t.Fatalf("租户=%v", got)
	}
	if got := reminderArgs[7].Value; got != int64(20) {
		t.Fatalf("批量上限=%v; want 20", got)
	}
}
