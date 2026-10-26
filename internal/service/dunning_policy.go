package service

import (
	"context"
	"fmt"
	"strings"
)

func (s *Service) CreateDunningPolicy(ctx context.Context, p Principal, in DunningPolicyInput) (string, error) {
	if textTooLong(in.Name, 255) {
		return "", fmt.Errorf("%w: policy name exceeds the length limit", ErrInvalid)
	}
	if in.AgingFromDays < 0 || in.AgingToDays < in.AgingFromDays || in.RepeatIntervalDays < 1 || !strings.HasPrefix(in.RecipientRule, "USER:") || (in.Priority != "NORMAL" && in.Priority != "HIGH" && in.Priority != "CRITICAL") {
		return "", ErrInvalid
	}
	if in.Channel != "LOCAL" && in.Channel != "PLATFORM" {
		return "", ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_dunning_policy(id,tenant_id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled,created_at,updated_at) VALUES(?,?,?,1,?,?,?,?,?,?,?,TRUE,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, in.Name, in.AgingFromDays, in.AgingToDays, in.ActionType, in.RecipientRule, in.Channel, in.RepeatIntervalDays, in.Priority)
	if err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_DUNNING_POLICY_CREATED", "dunning_policy", id, map[string]any{"actor_id": p.UserID, "result": "SUCCESS", "risk_level": "MEDIUM"}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}
