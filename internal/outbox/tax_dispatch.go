package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var errTaxCommandFinal = errors.New("tax command already has a final result")
var errTaxDispatchProtection = errors.New("tax dispatch protection failed")

// taxDispatchRecorder 在外部税控请求前保存结果未知事实。
type taxDispatchRecorder interface {
	// BeginTaxDispatch 在有效出站租约内保护税控额度和后续对账。
	// 参数：ctx为发送预算，events为本轮即将发送的税控命令。
	// 返回值：持久化成功为nil；终态命令收口后返回终态标记，其余错误失败关闭。
	BeginTaxDispatch(context.Context, []Event) error
}

// BeginTaxDispatch 先提交未知状态，再允许外部HTTP副作用，覆盖进程中断窗口。
// 参数：ctx为发送预算，events包含租户、出站ID和开票或红冲尝试引用。
// 返回值：无可信租约或数据库失败时拒绝发送；已终结命令收口并返回终态标记。
func (s *Store) BeginTaxDispatch(ctx context.Context, events []Event) error {
	for _, event := range events {
		if err := s.beginTaxEvent(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

// beginTaxEvent 按出站行到尝试行的固定锁序持久化一次税控发送意图。
// 参数：ctx为发送预算，event为具有当前Worker租约的税控事件。
// 返回值：事务错误或租约冲突；失败不会发送外部请求。
func (s *Store) beginTaxEvent(ctx context.Context, event Event) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var destination, status, owner, eventID string
	var live bool
	err = tx.QueryRowContext(ctx, `SELECT destination,status,COALESCE(locked_by,''),event_id,
	 locked_until>UTC_TIMESTAMP(3) FROM settlement_outbox_event
	 WHERE tenant_id=? AND id=? FOR UPDATE`, event.TenantID, event.ID).
		Scan(&destination, &status, &owner, &eventID, &live)
	if err != nil {
		return err
	}
	if destination != "TAX_INVOICE_COMMAND" || status != "PROCESSING" ||
		owner != s.WorkerID || eventID != event.EventID || !live {
		return ErrLeaseLost
	}
	err = markTaxAttemptUncertain(ctx, tx, event)
	if errors.Is(err, errTaxCommandFinal) {
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_outbox_event SET status='CANCELLED',
		 last_error_code='TAX_RESULT_ALREADY_FINAL',last_error_summary='税控结果已终结，停止重复发送',
		 locked_by=NULL,locked_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=?`,
			event.TenantID, event.ID); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return errTaxCommandFinal
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// markTaxAttemptUncertain 仅把首次发送尝试置UNKNOWN，不覆盖已知受理或终态。
// 参数：ctx/tx为发送事务，event为已核验租约的命令。
// 返回值：尝试引用不一致、终态或存储错误；UNKNOWN和ACCEPTED重试保持幂等。
func markTaxAttemptUncertain(ctx context.Context, tx *sql.Tx, event Event) error {
	var payload struct {
		AttemptID         string `json:"attempt_id"`
		RedFlushAttemptID string `json:"red_flush_attempt_id"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return err
	}
	table, id := "settlement_invoice_issue_attempt", payload.AttemptID
	if id == "" {
		table, id = "settlement_invoice_red_flush_attempt", payload.RedFlushAttemptID
	} else if payload.RedFlushAttemptID != "" {
		return errors.New("tax command has ambiguous attempt identity")
	}
	if id == "" {
		return errors.New("tax command has no attempt identity")
	}
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM `+table+
		` WHERE tenant_id=? AND id=? AND command_event_id=? FOR UPDATE`,
		event.TenantID, id, event.EventID).Scan(&status)
	if err != nil {
		return err
	}
	if status == "UNKNOWN" || status == "ACCEPTED" || status == "PROCESSING" {
		return nil
	}
	switch status {
	case "ISSUED", "ISSUE_FAILED", "RED_FLUSHED", "ABANDONED", "CANCELLED":
		return errTaxCommandFinal
	case "PENDING":
	default:
		return errors.New("tax attempt status is not dispatchable")
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+table+` SET status='UNKNOWN',
	 unknown_since=COALESCE(unknown_since,UTC_TIMESTAMP(3)),
	 next_reconcile_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 1 MINUTE)
	 WHERE tenant_id=? AND id=? AND status='PENDING'`, event.TenantID, id)
	return err
}

// deliverTaxEvents 逐命令发送并保存回执，隔离终态、回执故障及同行重试计数。
// 参数：ctx为批次预算，destination为税控目的地，events为已领租约事件，store为状态存储。
// 返回值：保护事务或状态记录错误；未发送同行保留租约等待恢复，不计投递失败。
func (w *Worker) deliverTaxEvents(ctx context.Context, destination Destination,
	events []Event, store eventStore,
) error {
	for _, event := range events {
		one := []Event{event}
		err := w.deliver(ctx, destination, one)
		if errors.Is(err, errTaxCommandFinal) {
			continue
		}
		if errors.Is(err, errTaxDispatchProtection) {
			return err
		}
		if errors.Is(err, ErrLicensePaused) {
			return nil
		}
		if err == nil {
			if err = store.Delivered(ctx, one); err != nil {
				return err
			}
			continue
		}
		var status *httpStatusError
		permanent := errors.As(err, &status) &&
			(status.Status == 400 || status.Status == 413 || status.Status == 422)
		if err = store.Failed(ctx, one, "DELIVERY_FAILED", err.Error(), permanent); err != nil {
			return err
		}
	}
	return nil
}
