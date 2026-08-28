package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func insertCreditSync(ctx context.Context, tx *sql.Tx, tenant, eventID, allocationID, receivableID string, payload []byte) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO settlement_credit_sync(id,tenant_id,event_id,allocation_id,receivable_id,status,payload_json,created_at,updated_at) VALUES(?,?,?,?,?,'PENDING',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newID(), tenant, eventID, allocationID, receivableID, payload)
	return err
}

// RetryCreditSync claims one event, sends it once, and records the result.
// It is intentionally invoked by the original request or a finance retry;
// no background scanner is involved.
func (s *Service) RetryCreditSync(ctx context.Context, tenant, eventID string) error {
	if s.CreditPublisher == nil {
		return errors.New("CRM credit publisher is not configured")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	var payload []byte
	err = tx.QueryRowContext(ctx, `SELECT id,payload_json FROM settlement_credit_sync WHERE tenant_id=? AND event_id=? AND status<>'DELIVERED' FOR UPDATE`, tenant, eventID).Scan(&id, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_credit_sync SET status='PROCESSING',locked_at=UTC_TIMESTAMP(3),last_attempt_at=UTC_TIMESTAMP(3),updated_at=UTC_TIMESTAMP(3) WHERE id=?`, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}

	if err = s.CreditPublisher.Publish(ctx, tenant, eventID, payload); err != nil {
		_, updateErr := s.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE settlement_credit_sync SET status='RETRY_WAIT',attempt_count=attempt_count+1,last_error_summary=?,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND status='PROCESSING'`, safeCreditError(err), id)
		if updateErr != nil {
			return fmt.Errorf("publish CRM credit event: %w; record failure: %v", err, updateErr)
		}
		return err
	}
	_, err = s.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE settlement_credit_sync SET status='DELIVERED',delivered_at=UTC_TIMESTAMP(3),last_error_summary='',updated_at=UTC_TIMESTAMP(3) WHERE id=? AND status='PROCESSING'`, id)
	return err
}

func safeCreditError(err error) string {
	value := err.Error()
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}
