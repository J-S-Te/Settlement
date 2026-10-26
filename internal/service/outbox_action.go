package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

func (s *Service) RetryOutboxEvent(ctx context.Context, p Principal, id string) error {
	if !p.Allows("settlement.admin") || strings.TrimSpace(id) == "" {
		return ErrForbidden
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM settlement_outbox_event WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if status != "DEAD_LETTER" {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE settlement_outbox_event SET status='PENDING',attempt_count=0,available_at=UTC_TIMESTAMP(3),locked_by=NULL,locked_until=NULL,last_error_code='',last_error_summary='',dead_lettered_at=NULL,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='DEAD_LETTER'`, p.TenantID, id); err != nil {
		return err
	}
	if err := outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_OUTBOX_MANUAL_RETRY", "outbox_event", id, map[string]any{"actor_id": p.UserID, "result": "SUCCESS", "risk_level": "MEDIUM"}); err != nil {
		return err
	}
	return tx.Commit()
}

type AgingBucket struct {
	Currency string `json:"currency"`
	Bucket   string `json:"bucket"`
	Amount   string `json:"amount"`
	Count    int64  `json:"count"`
}

// AgingSummary 在 SQL 中按币种和账龄段聚合每条未结清应收，
// 绝不使用浏览器当前加载页。
