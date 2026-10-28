package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

type ReceiptDifferenceInput struct {
	Key       string `json:"-"`
	ReceiptID string `json:"receipt_id"`
	Kind      string `json:"kind"`
	Amount    string `json:"amount"`
	Reason    string `json:"reason"`
}
type DifferenceResolutionInput struct {
	Version    int    `json:"version"`
	Resolution string `json:"resolution"`
	Reason     string `json:"reason"`
}

// financeAccess 校验财务操作权限及全租户业务范围。
// 参数：p 可信主体，permission 具体操作权限。
// 返回值：无权限或范围受限时返回拒绝。
func financeAccess(p Principal, permission string) error {
	if !p.Allows(permission) || !p.Unrestricted(permission) {
		return ErrForbidden
	}
	return nil
}

// CreateReceiptDifference 暂挂回款差异，对实际暂挂款扣减可核销余额。
// 参数：ctx 上下文，p 财务主体，in 差异事实。
// 返回值：差异标识或余额及校验错误。
func (s *Service) CreateReceiptDifference(ctx context.Context, p Principal, in ReceiptDifferenceInput) (string, error) {
	if err := financeAccess(p, "settlement.difference.manage"); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Key) == "" || len(in.Key) > 128 {
		return "", ErrInvalid
	}
	value, err := amount(in.Amount)
	if err != nil {
		return "", err
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || len(in.Reason) > 500 || (in.Kind != "OVERPAYMENT" && in.Kind != "SHORTPAYMENT" && in.Kind != "UNKNOWN") {
		return "", ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id := newID()
	if existing, replay, claimErr := claimIdempotency(ctx, tx, p.TenantID, "CREATE_RECEIPT_DIFFERENCE", in.Key, id, in); claimErr != nil {
		return "", claimErr
	} else if replay {
		return existing, nil
	}
	var available string
	if err = tx.QueryRowContext(ctx, `SELECT unallocated_amount FROM settlement_receipt WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, in.ReceiptID).Scan(&available); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if in.Kind != "SHORTPAYMENT" {
		if decimalGreater(value, available) {
			return "", ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_receipt SET unallocated_amount=unallocated_amount-?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=?`, value, p.TenantID, in.ReceiptID); err != nil {
			return "", err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receipt_difference(id,tenant_id,receipt_id,kind,amount,reason,status,requested_by,created_at) VALUES(?,?,?,?,?,?,'HELD',?,UTC_TIMESTAMP(3))`, id, p.TenantID, in.ReceiptID, in.Kind, value, in.Reason, p.UserID)
	if err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_DIFFERENCE_HELD", "receipt_difference", id, map[string]any{"actor_id": p.UserID}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// ResolveReceiptDifference 提交差异处理建议，退款仅记录待执行事实。
// 参数：ctx 上下文，p 财务主体，id 差异标识，in 版本及处置原因。
// 返回值：版本冲突或处理错误。
func (s *Service) ResolveReceiptDifference(ctx context.Context, p Principal, id string, in DifferenceResolutionInput) error {
	if err := financeAccess(p, "settlement.difference.manage"); err != nil {
		return err
	}
	if (in.Resolution != "REFUND_PENDING" && in.Resolution != "CARRY_FORWARD" && in.Resolution != "MATCHED") || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 500 {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE settlement_receipt_difference SET resolution=?,resolution_reason=?,resolved_by=?,status='REVIEW_PENDING',version=version+1 WHERE tenant_id=? AND id=? AND status='HELD' AND version=? AND (kind<>'SHORTPAYMENT' OR ?='MATCHED')`, in.Resolution, in.Reason, p.UserID, p.TenantID, id, in.Version, in.Resolution)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_DIFFERENCE_RESOLVED", "receipt_difference", id, map[string]any{"actor_id": p.UserID, "reason": in.Reason}); err != nil {
		return err
	}
	return tx.Commit()
}

// ApproveReceiptDifference 非申请人及非处置人复核差异，并释放非退款暂挂余额。
// 参数：ctx 上下文，p 复核主体，id 差异标识，version 预期版本。
// 返回值：授权、状态或持久化错误。
func (s *Service) ApproveReceiptDifference(ctx context.Context, p Principal, id string, version int) error {
	if err := financeAccess(p, "settlement.difference.approve"); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var applicant, resolver, receipt, kind, resolution, value string
	if err = tx.QueryRowContext(ctx, `SELECT requested_by,resolved_by,receipt_id,kind,resolution,amount FROM settlement_receipt_difference WHERE tenant_id=? AND id=? AND status='REVIEW_PENDING' AND version=? FOR UPDATE`, p.TenantID, id, version).Scan(&applicant, &resolver, &receipt, &kind, &resolution, &value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConflict
		}
		return err
	}
	if applicant == p.UserID || resolver == p.UserID {
		return ErrForbidden
	}
	state := "APPROVED"
	if resolution == "REFUND_PENDING" {
		state = "REFUND_PENDING"
	} else if kind != "SHORTPAYMENT" {
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_receipt SET unallocated_amount=unallocated_amount+?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=?`, value, p.TenantID, receipt); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_receipt_difference SET status=?,approved_by=?,version=version+1 WHERE tenant_id=? AND id=?`, state, p.UserID, p.TenantID, id); err != nil {
		return err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_DIFFERENCE_APPROVED", "receipt_difference", id, map[string]any{"actor_id": p.UserID, "resolution": resolution}); err != nil {
		return err
	}
	return tx.Commit()
}

type ERPProvider interface {
	// SendVoucher 按本地凭证标识幂等回传ERP。
	// 参数：上下文、稳定幂等标识、原始凭证JSON。
	// 返回值：真实外部凭证编号或错误；超时重试必须返回同一结果。
	SendVoucher(context.Context, string, json.RawMessage) (string, error)
}

// CreateAccountingVoucher 从已确认核销生成本地凭证，幂等于来源核销。
// 参数：ctx 上下文，p 财务主体，writeoffID 已确认核销标识。
// 返回值：本地凭证标识或校验及存储错误。
func (s *Service) CreateAccountingVoucher(ctx context.Context, p Principal, writeoffID string) (string, error) {
	if err := financeAccess(p, "settlement.voucher.manage"); err != nil {
		return "", err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var receipt, receivable, value, currency string
	if err = tx.QueryRowContext(ctx, `SELECT receipt_id,receivable_id,allocated_amount,currency FROM settlement_receipt_allocation WHERE tenant_id=? AND id=? AND status='CONFIRMED' FOR UPDATE`, p.TenantID, writeoffID).Scan(&receipt, &receivable, &value, &currency); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrConflict
		}
		return "", err
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_accounting_voucher WHERE tenant_id=? AND writeoff_id=?`, p.TenantID, writeoffID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id := newID()
	payload, _ := json.Marshal(map[string]string{"receipt_id": receipt, "receivable_id": receivable, "amount": value, "currency": currency, "writeoff_id": writeoffID})
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_accounting_voucher(id,tenant_id,writeoff_id,payload,status,created_at) VALUES(?,?,?,?,'NOT_CONFIGURED',UTC_TIMESTAMP(3))`, id, p.TenantID, writeoffID, payload)
	if err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_VOUCHER_CREATED", "accounting_voucher", id, map[string]any{"actor_id": p.UserID}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// RetryAccountingVoucher 以稳定凭证标识重试ERP并保存真实结果。
// 参数：ctx 上下文，p 财务主体，id 本地凭证标识。
// 返回值：NOT_CONFIGURED、FAILED或SENT及存储错误。
func (s *Service) RetryAccountingVoucher(ctx context.Context, p Principal, id string) (string, error) {
	if err := financeAccess(p, "settlement.voucher.manage"); err != nil {
		return "", err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var payload json.RawMessage
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT payload,status FROM settlement_accounting_voucher WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, id).Scan(&payload, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if status == "SENT" {
		return status, nil
	}
	external := ""
	status = "NOT_CONFIGURED"
	if s.ERPProvider != nil {
		status = "FAILED"
		callCtx, cancel := context.WithTimeout(ctx, 30*1000000000)
		defer cancel()
		external, err = s.ERPProvider.SendVoucher(callCtx, id, payload)
		if err == nil && external != "" && len(external) <= 128 {
			status = "SENT"
		} else {
			external = ""
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_accounting_voucher SET status=?,external_no=?,attempts=attempts+1,version=version+1 WHERE tenant_id=? AND id=?`, status, external, p.TenantID, id); err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_VOUCHER_"+status, "accounting_voucher", id, map[string]any{"actor_id": p.UserID}); err != nil {
		return "", err
	}
	return status, tx.Commit()
}
