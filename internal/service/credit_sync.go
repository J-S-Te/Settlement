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

// insertCreditSyncEvent 将历史结清事件暂存，普通事件维持待投递。
// 参数：ctx为上下文，tx为账务事务，其余参数为事件和业务引用及不可变内容。
// 返回值：持久化错误。
func insertCreditSyncEvent(ctx context.Context, tx *sql.Tx, tenant, eventID, allocationID, receivableID, eventType, originalEventID string, payload []byte) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO settlement_credit_sync(id,tenant_id,event_id,event_type,original_event_id,allocation_id,receivable_id,status,payload_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?, (SELECT CASE WHEN EXISTS(SELECT 1 FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id WHERE r.tenant_id=? AND r.id=? AND cs.migration_state='HOLD') THEN 'HOLD' ELSE 'PENDING' END),?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newID(), tenant, eventID, eventType, originalEventID, allocationID, receivableID, tenant, receivableID, payload)
	return err
}

// RetryCreditSync 认领并重试信用同步，历史HOLD保持不可投递。
// 参数：ctx为上下文，tenant/eventID为可信租户及目标信用事件。
// 返回值：同步结果错误；历史暂存事件不会对外发送。
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
	err = tx.QueryRowContext(ctx, `SELECT id,payload_json FROM settlement_credit_sync WHERE tenant_id=? AND event_id=? AND status IN ('PENDING','RETRY_WAIT') AND NOT EXISTS(SELECT 1 FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id WHERE r.id=settlement_credit_sync.receivable_id AND r.tenant_id=settlement_credit_sync.tenant_id AND cs.migration_state='HOLD') FOR UPDATE`, tenant, eventID).Scan(&id, &payload)
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
