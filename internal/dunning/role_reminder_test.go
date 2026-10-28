package dunning

import (
	"context"
	"errors"
	"github.com/j-s-te/settlement/internal/service"
	"testing"
)

// TestRoleReminderPhase 验证默认提前三天和逾期第一天的触发边界。
// 参数：t 测试上下文。
// 返回值：无。
func TestRoleReminderPhase(t *testing.T) {
	rules := service.ReminderRules{LeadDays: 3, RepeatDays: 7, EscalationDays: 30}
	for _, c := range []struct {
		aging   int
		pending bool
		want    string
	}{{-4, false, ""}, {-3, false, "DUE_SOON"}, {0, false, "DUE_SOON"}, {1, false, "OVERDUE"}, {30, false, "ESCALATED"}, {-2, true, "PLAN_CONFIRMATION"}, {1, true, "OVERDUE"}, {30, true, "ESCALATED"}} {
		if got := reminderPhase(c.aging, c.pending, rules); got != c.want {
			t.Fatalf("%+v got %s", c, got)
		}
	}
}

// TestRoleReminderFailsClosedWithoutDirectory 验证目录缺失不能继续生成提醒。
// 参数：t 测试上下文。
// 返回值：无。
func TestRoleReminderFailsClosedWithoutDirectory(t *testing.T) {
	if err := (&RoleReminderWorker{TenantID: "tenant"}).RunOnce(context.Background()); err == nil {
		t.Fatal("missing directory accepted")
	}
	denied := errors.New("license denied")
	if err := (&RoleReminderWorker{CheckBusinessLicense: func(context.Context) error { return denied }}).RunOnce(context.Background()); !errors.Is(err, denied) {
		t.Fatal(err)
	}
}
