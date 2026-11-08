package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// CancellableInvoiceRequestStatuses 是允许作废的申请状态。只有"确定没开出去"
// 的申请才能回收额度：仍在开票中（ISSUE_PENDING）或已明确失败（ISSUE_FAILED）。
var cancellableInvoiceRequestStatuses = map[string]bool{"ISSUE_PENDING": true, "ISSUE_FAILED": true}

// cancellableAttemptStatuses 是允许作废的尝试状态。UNKNOWN、ACCEPTED、
// PROCESSING、ISSUED 都意味着税控可能已经受理，一律不得释放额度。
var cancellableAttemptStatuses = map[string]bool{"PENDING": true, "ISSUE_FAILED": true}

// CancelInvoiceRequest 用途：作废确定未被税控受理的开票申请并释放额度预留。
// 只有"最新尝试从未被税控接受、命令已死信或从未下发、没有发票、没有未知或冲突结果"
// 才允许作废；任一条件无法证明即失败关闭，引导人工先对账或红冲。
// 相同幂等键重放返回首次结果，不重复改状态、释放额度或写审计。
// 参数：ctx为上下文，p为可信主体，key为幂等键，id为开票申请，version为当前版本，reason为作废原因。
// 返回值：权限、状态机、冲突或存储错误；成功时已提交。
func (s *Service) CancelInvoiceRequest(ctx context.Context, p Principal, key, id string, version int, reason string) error {
	if !p.Allows("settlement.invoice.issue") || strings.TrimSpace(p.TenantID) == "" || strings.TrimSpace(p.UserID) == "" {
		return ErrForbidden
	}
	id = strings.TrimSpace(id)
	key = strings.TrimSpace(key)
	reason = strings.TrimSpace(reason)
	if id == "" || key == "" || len(key) > 128 || version < 1 || len([]rune(reason)) < 5 || len([]rune(reason)) > 1000 {
		return fmt.Errorf("%w: 作废原因应为 5 到 1000 字，且必须提供当前版本与幂等键", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, replay, err := claimIdempotency(ctx, tx, p.TenantID, "CANCEL_INVOICE_REQUEST", key, id, map[string]any{"actor_id": p.UserID, "id": id, "version": version, "reason": reason})
	if err != nil {
		return err
	}
	if replay {
		return tx.Commit()
	}
	// 加锁顺序固定为 命令死信 → 尝试 → 申请，与人工重投（outbox → attempt → request）
	// 保持一致，避免"重投×作废"交叉加锁；先做一次不加锁定位，随后逐层复核。
	var attemptID, commandEventID string
	err = tx.QueryRowContext(ctx, `SELECT id,COALESCE(command_event_id,'') FROM settlement_invoice_issue_attempt WHERE tenant_id=? AND invoice_request_id=? ORDER BY attempt_no DESC,requested_at DESC,id DESC LIMIT 1`, p.TenantID, id).Scan(&attemptID, &commandEventID)
	if errors.Is(err, sql.ErrNoRows) {
		// 从未创建税控尝试的申请（例如人工渠道申请）没有命令可作废，
		// 仍需按申请状态与发票事实判定。
		attemptID, commandEventID = "", ""
	} else if err != nil {
		return err
	}
	if commandEventID != "" {
		var destination, commandStatus string
		err = tx.QueryRowContext(ctx, `SELECT destination,status FROM settlement_outbox_event WHERE tenant_id=? AND event_id=? ORDER BY created_at DESC,id DESC LIMIT 1 FOR UPDATE`, p.TenantID, commandEventID).Scan(&destination, &commandStatus)
		if errors.Is(err, sql.ErrNoRows) {
			// 命令事件缺失意味着无法证明税控从未受理，禁止释放额度。
			return fmt.Errorf("%w: 税控命令事实缺失，禁止作废，请先对账", ErrConflict)
		}
		if err != nil {
			return err
		}
		// 与重投互斥：命令被重投后状态回到 PENDING，必须先等结果收敛。
		if destination != "TAX_INVOICE_COMMAND" || commandStatus != "DEAD_LETTER" {
			return fmt.Errorf("%w: 税控命令仍在投递或已被重投，请先确认税控结果", ErrConflict)
		}
	}
	if attemptID != "" {
		var attemptStatus, lockedCommand string
		var neverAccepted bool
		err = tx.QueryRowContext(ctx, `SELECT status,(accepted_at IS NULL),COALESCE(command_event_id,'') FROM settlement_invoice_issue_attempt WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, attemptID).Scan(&attemptStatus, &neverAccepted, &lockedCommand)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if lockedCommand != commandEventID {
			return fmt.Errorf("%w: 开票尝试的命令已变化，请刷新后重试", ErrConflict)
		}
		if !neverAccepted {
			return fmt.Errorf("%w: 税控已受理该尝试，禁止作废，请先对账或红冲", ErrConflict)
		}
		// 旧版本在发送后丢失回执仍可能留下PENDING；死信不能证明从未发送。
		if attemptStatus == "PENDING" && commandEventID != "" {
			return fmt.Errorf("%w: 税控命令缺少确定未受理证据，请先对账或恢复投递", ErrConflict)
		}
		if !cancellableAttemptStatuses[attemptStatus] {
			return fmt.Errorf("%w: 开票尝试状态为 %s，无法证明未受理，禁止作废", ErrConflict, attemptStatus)
		}
	}
	var status, applicant string
	var currentVersion int
	err = tx.QueryRowContext(ctx, `SELECT status,submitted_by,version FROM settlement_invoice_request WHERE id=? AND tenant_id=? FOR UPDATE`, id, p.TenantID).Scan(&status, &applicant, &currentVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !cancellableInvoiceRequestStatuses[status] || currentVersion != version {
		return fmt.Errorf("%w: 只有开票中或已失败且版本一致的申请可以作废", ErrConflict)
	}
	// 与失败复核一致的申请人回避：作废会释放额度，必须由他人执行。
	if applicant == p.UserID {
		return fmt.Errorf("%w: 申请人不能作废自己的开票申请", ErrInvalid)
	}
	if err = s.requireInvoiceRequestScopeTx(ctx, tx, p, "settlement.invoice.issue", id); err != nil {
		return err
	}
	var invoices int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_tax_invoice WHERE invoice_request_id=? AND tenant_id=?`, id, p.TenantID).Scan(&invoices); err != nil {
		return err
	}
	if invoices != 0 {
		return fmt.Errorf("%w: 申请已存在发票，禁止作废", ErrConflict)
	}
	// 未知结果与厂商冲突都表示"可能已开票"，fail-closed 不得释放额度。
	var unresolved int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_tax_result_inbox ti JOIN settlement_invoice_issue_attempt ia ON ia.id=ti.aggregate_id AND ia.tenant_id=ti.tenant_id WHERE ia.invoice_request_id=? AND ia.tenant_id=? AND ti.operation_type='INVOICE_ISSUE' AND ti.processing_status IN ('UNKNOWN','PROVIDER_CONFLICT')`, id, p.TenantID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved != 0 {
		return fmt.Errorf("%w: 税控结果存在未知或冲突，禁止作废，请先对账", ErrConflict)
	}
	if attemptID != "" {
		result, execErr := tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET status='ABANDONED' WHERE tenant_id=? AND id=? AND status IN ('PENDING','ISSUE_FAILED')`, p.TenantID, attemptID)
		if execErr != nil {
			return execErr
		}
		affected, execErr := result.RowsAffected()
		if execErr != nil {
			return execErr
		}
		if affected != 1 {
			return ErrConflict
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='CANCELLED',rejection_reason=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND version=? AND status IN ('ISSUE_PENDING','ISSUE_FAILED')`, reason, id, p.TenantID, version)
	if err != nil {
		return err
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if affected != 1 {
		return ErrConflict
	}
	// 只释放"预留但未开票"的差额；已开票部分永不回收。
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request_allocation SET released_amount=reserved_amount-invoiced_amount,status='RELEASED' WHERE tenant_id=? AND invoice_request_id=? AND status='RESERVED'`, p.TenantID, id); err != nil {
		return err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_CANCELLED", "invoice_request", id, map[string]any{"actor_id": p.UserID, "attempt_id": attemptID, "command_event_id": commandEventID, "reason": reason, "previous_status": status, "previous_version": version, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return err
	}
	return tx.Commit()
}
