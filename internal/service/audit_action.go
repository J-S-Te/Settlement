package service

import (
	"context"
)

func (s *Service) AuditAction(ctx context.Context, p Principal, eventType, resourceType, resourceID string, detail map[string]any) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if detail == nil {
		detail = map[string]any{}
	}
	// 通用 HTTP 审计会传入失败/拒绝结果；缺省仍保持既有调用方的成功语义。
	result := "SUCCESS"
	if value, ok := detail["result"].(string); ok && value != "" {
		result = value
	}
	detail["actor_id"] = p.UserID
	detail["result"] = result
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", eventType, resourceType, resourceID, detail); err != nil {
		return err
	}
	return tx.Commit()
}

type ReversalInput struct {
	Amount       string `json:"amount"`
	ReasonCode   string `json:"reason_code"`
	ReasonDetail string `json:"reason_detail"`
}
