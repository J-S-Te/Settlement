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
	detail["actor_id"] = p.UserID
	detail["result"] = "SUCCESS"
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
