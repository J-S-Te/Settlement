package service

import "context"

func (s *Service) ListOutboxEvents(ctx context.Context, p Principal) ([]OutboxEvent, error) {
	if !p.Allows("settlement.admin") {
		return nil, ErrForbidden
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,event_id,destination,event_type,status,attempt_count,
		DATE_FORMAT(available_at,'%Y-%m-%dT%H:%i:%s.%fZ'),last_error_code,last_error_summary,
		COALESCE(DATE_FORMAT(dead_lettered_at,'%Y-%m-%dT%H:%i:%s.%fZ'),''),DATE_FORMAT(created_at,'%Y-%m-%dT%H:%i:%s.%fZ')
		FROM settlement_outbox_event WHERE tenant_id=? AND status IN ('RETRY_WAIT','DEAD_LETTER')
		ORDER BY created_at DESC,id DESC LIMIT 200`, p.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxEvent{}
	for rows.Next() {
		var item OutboxEvent
		if err := rows.Scan(&item.ID, &item.EventID, &item.Destination, &item.EventType, &item.Status, &item.AttemptCount, &item.AvailableAt, &item.LastErrorCode, &item.LastErrorSummary, &item.DeadLetteredAt, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// RetryOutboxEvent 在显式管理员命令下重置一条死信
// 事件，绝不编辑载荷或事件 ID，以保留投递身份。
