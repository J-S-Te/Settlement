package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ConfirmRefundRequest 记录银行等外部渠道已完成退款的事实。
type ConfirmRefundRequest struct {
	Version     int    `json:"version"`
	Reference   string `json:"reference"`
	CompletedAt string `json:"completed_at"`
	Reason      string `json:"reason"`
}

// normalizeRefundConfirmation 校验回录凭证和完成时间，统一为UTC。
// 参数：in 回录事实，now 校验使用的当前时间。
// 返回值：规范化事实及UTC完成时间；无效输入返回 ErrInvalid。
func normalizeRefundConfirmation(in ConfirmRefundRequest, now time.Time) (ConfirmRefundRequest, time.Time, error) {
	in.Reference = strings.TrimSpace(in.Reference)
	in.Reason = strings.TrimSpace(in.Reason)
	completed, err := time.Parse(time.RFC3339, in.CompletedAt)
	if err != nil || in.Version < 1 || in.Reference == "" || len(in.Reference) > 128 || in.Reason == "" || len(in.Reason) > 500 || completed.After(now) {
		return in, time.Time{}, ErrInvalid
	}
	return in, completed.UTC(), nil
}

// ConfirmReceiptRefund 原子回录独立审批后已完成的外部退款，不改变回款余额。
// 参数：ctx 上下文，p 财务主体，id 差异标识，in 版本及外部退款事实。
// 返回值：越权、缺失、状态版本冲突、输入错误或持久化错误。
func (s *Service) ConfirmReceiptRefund(ctx context.Context, p Principal, id string, in ConfirmRefundRequest) error {
	if err := financeAccess(p, "settlement.difference.manage"); err != nil {
		return err
	}
	in, completed, err := normalizeRefundConfirmation(in, time.Now())
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockApprovedRefund(ctx, tx, p.TenantID, id, in.Version, completed); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE settlement_receipt_difference
 SET status='REFUND_CONFIRMED',refund_reference=?,refund_completed_at=?,
 refund_confirmed_by=?,refund_reason=?,version=version+1
 WHERE tenant_id=? AND id=? AND version=? AND status='REFUND_PENDING'`,
		in.Reference, completed.Format("2006-01-02 15:04:05.000"), p.UserID, in.Reason, p.TenantID, id, in.Version)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrConflict
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_REFUND_CONFIRMED", "receipt_difference", id, map[string]any{"actor_id": p.UserID, "reference": in.Reference, "completed_at": completed, "reason": in.Reason}); err != nil {
		return err
	}
	return tx.Commit()
}

// lockApprovedRefund 锁定同租户差异与回款，核验独立审批和日期边界。
// 参数：ctx 上下文，tx 当前事务，tenant 租户，id 差异，version 版本，completed UTC时间。
// 返回值：不可见对象返回 ErrNotFound，状态冲突返回 ErrConflict，日期非法返回 ErrInvalid。
func lockApprovedRefund(ctx context.Context, tx *sql.Tx, tenant, id string, version int, completed time.Time) error {
	var status, resolution, applicant, resolver, approver, receipt string
	var current int
	err := tx.QueryRowContext(ctx, `SELECT status,resolution,requested_by,resolved_by,approved_by,receipt_id,version
 FROM settlement_receipt_difference WHERE tenant_id=? AND id=? FOR UPDATE`, tenant, id).
		Scan(&status, &resolution, &applicant, &resolver, &approver, &receipt, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current != version || status != "REFUND_PENDING" || resolution != "REFUND_PENDING" || strings.TrimSpace(approver) == "" || approver == applicant || approver == resolver {
		return ErrConflict
	}
	var date string
	err = tx.QueryRowContext(ctx, `SELECT DATE_FORMAT(receipt_date,'%Y-%m-%d') FROM settlement_receipt
 WHERE tenant_id=? AND id=? FOR UPDATE`, tenant, receipt).Scan(&date)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	receiptDate, err := time.Parse("2006-01-02", date)
	if err != nil {
		return err
	}
	if completed.Before(receiptDate) {
		return ErrInvalid
	}
	return nil
}
