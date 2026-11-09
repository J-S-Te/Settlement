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

// original_event_id 是同应收内部前驱，不进入 CRM 消息；撤销引用原完成，
// 下一次完成引用前轮撤销，只有确认前驱受理后才允许继续发送。
const creditPredecessorReady = `(original_event_id='' OR EXISTS (
 SELECT 1 FROM settlement_credit_sync predecessor
 WHERE predecessor.tenant_id=settlement_credit_sync.tenant_id
 AND predecessor.receivable_id=settlement_credit_sync.receivable_id
 AND predecessor.event_id=settlement_credit_sync.original_event_id
 AND predecessor.status='DELIVERED'))`

// ErrCreditSyncAuthorization 表示 CRM 信用同步被凭据或用途授权拒绝（401/403、
// invalid_client/invalid_scope）。它与 5xx/网络类失败语义不同：修复受控接入配置
// 之前重试不会成功，HTTP 层必须让运营看到不可立即重试的明确错误，而不是 503。
var ErrCreditSyncAuthorization = errors.New("CRM credit delivery is not authorized")

// insertCreditSync 将新完成排在前轮撤销之后，保持每个应收的不可变事实顺序。
// 参数：ctx/tx为财务事务，其余为租户、事件、核销、应收身份和事件内容。
// 返回值：前驱查询或事件持久化错误，成功时无错误。
func insertCreditSync(ctx context.Context, tx *sql.Tx,
	tenant, eventID, allocationID, receivableID string, payload []byte,
) error {
	var predecessor string
	err := tx.QueryRowContext(ctx, `SELECT reversed.event_id
	 FROM settlement_credit_sync reversed
	 WHERE reversed.tenant_id=? AND reversed.receivable_id=? AND reversed.event_type=?
	 AND NOT EXISTS (SELECT 1 FROM settlement_credit_sync following
	  WHERE following.tenant_id=reversed.tenant_id
	  AND following.original_event_id=reversed.event_id
	  AND following.event_type=? AND following.status<>'CANCELLED')
	 ORDER BY reversed.created_at DESC,reversed.id DESC LIMIT 1 FOR UPDATE`,
		tenant, receivableID, creditEventReversed, creditEventCompleted).Scan(&predecessor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return insertCreditSyncEvent(ctx, tx, tenant, eventID, allocationID, receivableID,
		creditEventCompleted, predecessor, payload)
}

// insertCreditSyncEvent 将历史结清事件暂存，普通事件维持待投递。
// 参数：ctx为上下文，tx为账务事务，其余参数为事件和业务引用及不可变内容。
// 返回值：持久化错误。
func insertCreditSyncEvent(ctx context.Context, tx *sql.Tx, tenant, eventID, allocationID, receivableID, eventType, originalEventID string, payload []byte) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO settlement_credit_sync(id,tenant_id,event_id,event_type,original_event_id,allocation_id,receivable_id,status,payload_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?, (SELECT CASE WHEN EXISTS(SELECT 1 FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id WHERE r.tenant_id=? AND r.id=? AND cs.migration_state='HOLD') THEN 'HOLD' ELSE 'PENDING' END),?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newID(), tenant, eventID, eventType, originalEventID, allocationID, receivableID, tenant, receivableID, payload)
	return err
}

// RetryCreditSync 仅在前驱已确认时认领重试信用同步，历史HOLD保持不可投递。
// 参数：ctx为上下文，tenant/eventID为可信租户及目标信用事件。
// 返回值：同步结果错误；历史暂存事件不会对外发送。
func (s *Service) RetryCreditSync(ctx context.Context, tenant, eventID string) error {
	if !s.creditSubscribed() {
		return ErrCreditSyncNotSubscribed
	}
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
	err = tx.QueryRowContext(ctx, `SELECT id,payload_json FROM settlement_credit_sync
	 WHERE tenant_id=? AND event_id=? AND status IN ('PENDING','RETRY_WAIT')
	 AND `+creditPredecessorReady+` AND NOT EXISTS(
	  SELECT 1 FROM settlement_receivable r JOIN settlement_contract_snapshot cs
	  ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id
	  WHERE r.id=settlement_credit_sync.receivable_id
	  AND r.tenant_id=settlement_credit_sync.tenant_id AND cs.migration_state='HOLD')
	 FOR UPDATE`, tenant, eventID).Scan(&id, &payload)
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
		if creditclient.AuthorizationDenied(err) {
			// 授权/用途错误保留 RETRY_WAIT 供修复接入配置后显式重试，但返回可分类
			// 错误，避免调用方把配置漂移当作瞬时 5xx 反复重试。
			return fmt.Errorf("%w: %v", ErrCreditSyncAuthorization, err)
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
