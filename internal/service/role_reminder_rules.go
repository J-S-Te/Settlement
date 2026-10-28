package service

import "context"

type ReminderRules struct{ LeadDays, RepeatDays, EscalationDays int }

// ReminderRules 读取经校验的提醒运行规则，复用同一设置来源。
// 参数：ctx 上下文，tenant 可信租户标识。
// 返回值：提醒提前量、重复及升级天数和存储错误。
func (s *Service) ReminderRules(ctx context.Context, tenant string) (ReminderRules, error) {
	in, err := s.loadSettlementSettings(ctx, tenant)
	return ReminderRules{in.ReminderLeadDays, in.RepeatDays, in.EscalationDays}, err
}
