package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/j-s-te/settlement/internal/creditclient"
)

const creditEventCompleted = "customer.credit.payment.completed.v1"
const creditEventReversed = "customer.credit.payment.reversed.v1"

func insertCreditSync(ctx context.Context, tx *sql.Tx, tenant, eventID, allocationID, receivableID string, payload []byte) error {
	return insertCreditSyncEvent(ctx, tx, tenant, eventID, allocationID, receivableID, creditEventCompleted, "", payload)
}

func insertCreditSyncEvent(ctx context.Context, tx *sql.Tx, tenant, eventID, allocationID, receivableID, eventType, originalEventID string, payload []byte) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO settlement_credit_sync(id,tenant_id,event_id,event_type,original_event_id,allocation_id,receivable_id,status,payload_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?, 'PENDING',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newID(), tenant, eventID, eventType, originalEventID, allocationID, receivableID, payload)
	return err
}

// RetryCreditSync 认领一个事件，发送一次并记录结果。
// 有意由原请求或财务重试触发，
// 不涉及后台扫描器。
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
		if errors.Is(err, creditclient.ErrLicensePaused) {
			// 尚未发送任何内容。保留认领租约以便正常恢复，
			// 不能将许可证暂停计为 CRM 投递失败。
			return err
		}
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
