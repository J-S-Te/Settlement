package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ApproveInvoiceRequest 执行财务审批并在需要时转总监加签。
// 参数：ctx 上下文，p 审批主体，id 申请标识，version 预期版本。
// 返回值：执行任务标识（等待加签时为空）或错误。
func (s *Service) ApproveInvoiceRequest(ctx context.Context, p Principal, id string, version int) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var applicant string
	err = tx.QueryRowContext(ctx, `SELECT submitted_by FROM settlement_invoice_request WHERE id=? AND tenant_id=? AND status='SUBMITTED' AND version=? FOR UPDATE`, id, p.TenantID, version).Scan(&applicant)
	if err != nil {
		return "", ErrConflict
	}
	if applicant == p.UserID {
		return "", fmt.Errorf("%w: applicant cannot approve own invoice", ErrInvalid)
	}
	var special bool
	err = tx.QueryRowContext(ctx, `SELECT required_review FROM settlement_invoice_special_review WHERE tenant_id=? AND invoice_request_id=?`, p.TenantID, id).Scan(&special)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if special {
		_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='SPECIAL_REVIEW',approved_by=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND version=?`, p.UserID, id, p.TenantID, version)
		if err != nil {
			return "", err
		}
		if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_SPECIAL_REVIEW_PENDING", "invoice_request", id, map[string]any{"actor_id": p.UserID}); err != nil {
			return "", err
		}
		return "", tx.Commit()
	}
	return s.createApprovedInvoiceAttempt(ctx, tx, p, id, version)
}

// createApprovedInvoiceAttempt 创建已完成审批的执行任务。
// 参数：ctx 上下文，tx 事务，p 审批主体，id 申请标识，version 当前版本。
// 返回值：执行任务标识或错误；成功提交事务。
func (s *Service) createApprovedInvoiceAttempt(ctx context.Context, tx *sql.Tx, p Principal, id string, version int) (string, error) {
	var err error
	// 与较早明确失败矛盾的迟到成功必须
	// 对账，不能随后重新尝试开具。
	var providerConflicts int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_tax_result_inbox ti JOIN settlement_invoice_issue_attempt ia ON ia.id=ti.aggregate_id AND ia.tenant_id=ti.tenant_id WHERE ia.invoice_request_id=? AND ia.tenant_id=? AND ti.operation_type='INVOICE_ISSUE' AND ti.processing_status='PROVIDER_CONFLICT'`, id, p.TenantID).Scan(&providerConflicts); err != nil {
		return "", err
	}
	if providerConflicts != 0 {
		return "", fmt.Errorf("%w: 税控结果存在冲突，禁止重新开票", ErrConflict)
	}
	attempt := newID()
	channel := "MANUAL"
	if s.invoiceIssuanceMode() == "tax_adapter" {
		channel = "TAX_ADAPTER"
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='ISSUE_PENDING',approved_by=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND version=?`, p.UserID, id, p.TenantID, version)
	if err != nil {
		return "", err
	}
	providerCode := ""
	if channel == "TAX_ADAPTER" {
		providerCode = s.taxProviderCode()
	}
	var attemptNo int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt_no),0)+1 FROM settlement_invoice_issue_attempt WHERE invoice_request_id=? AND tenant_id=?`, id, p.TenantID).Scan(&attemptNo); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_issue_attempt(id,tenant_id,invoice_request_id,attempt_no,channel,provider_code,external_request_id,idempotency_key,status,requested_at) VALUES(?,?,?,?,?,?,?,?,'PENDING',UTC_TIMESTAMP(3))`, attempt, p.TenantID, id, attemptNo, channel, providerCode, attempt, attempt)
	if err != nil {
		return "", err
	}
	if channel == "TAX_ADAPTER" {
		command, commandErr := invoiceIssueCommand(ctx, tx, p.TenantID, id, attempt, s.taxProviderCode())
		if commandErr != nil {
			return "", commandErr
		}
		if err = outbox(ctx, tx, p.TenantID, "TAX_INVOICE_COMMAND", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "invoice_request", id, command); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET command_event_id=(SELECT event_id FROM settlement_outbox_event WHERE tenant_id=? AND destination='TAX_INVOICE_COMMAND' AND event_type='SETTLEMENT_INVOICE_ISSUE_REQUESTED' AND aggregate_id=? ORDER BY created_at DESC,id DESC LIMIT 1) WHERE id=? AND tenant_id=?`, p.TenantID, id, attempt, p.TenantID); err != nil {
			return "", err
		}
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_APPROVED", "invoice_request", id, map[string]any{"actor_id": p.UserID, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return attempt, tx.Commit()
}

// RejectInvoiceRequest 拒绝普通申请并释放余额预留。
// 参数：ctx 上下文，p 审核主体，id 申请标识，version 版本，reason 原因。
// 返回值：状态冲突或处理错误。
func (s *Service) RejectInvoiceRequest(ctx context.Context, p Principal, id string, version int, reason string) error {
	reason, err := required(reason, "reason")
	if err != nil {
		return err
	}
	if len(reason) > 500 {
		return fmt.Errorf("%w: reason is too long", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var applicant string
	err = tx.QueryRowContext(ctx, `SELECT submitted_by FROM settlement_invoice_request WHERE id=? AND tenant_id=? AND status='SUBMITTED' AND version=? FOR UPDATE`, id, p.TenantID, version).Scan(&applicant)
	if err != nil {
		return ErrConflict
	}
	if applicant == p.UserID {
		return fmt.Errorf("%w: applicant cannot review own invoice", ErrInvalid)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='REJECTED',approved_by=?,rejection_reason=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND version=?`, p.UserID, reason, id, p.TenantID, version); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request_allocation SET released_amount=reserved_amount,status='RELEASED' WHERE tenant_id=? AND invoice_request_id=? AND status='RESERVED'`, p.TenantID, id); err != nil {
		return err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_REJECTED", "invoice_request", id, map[string]any{"actor_id": p.UserID, "reason": reason, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return err
	}
	return tx.Commit()
}

type ManualInvoiceInput struct {
	InvoiceCode   string         `json:"invoice_code"`
	InvoiceNo     string         `json:"invoice_no"`
	IssueDate     string         `json:"issue_date"`
	SellerProfile map[string]any `json:"seller_profile"`
}

// RegisterManualInvoice 是安全的第一阶段适配器：可关闭
// 已审批请求，无需将核心台账耦合到特定税控厂商。

// ReviewSpecialInvoice 执行财务之后的总监加签并禁止申请人自审。
// 参数：ctx 上下文，p 总监主体，id 申请标识，version 预期版本，approve 决定，reason 拒绝原因。
// 返回值：加签通过后的执行任务标识或错误。
func (s *Service) ReviewSpecialInvoice(ctx context.Context, p Principal, id string, version int, approve bool, reason string) (string, error) {
	if !p.Allows("settlement.invoice.special_approve") {
		return "", ErrForbidden
	}
	if !approve && (len(reason) > 500 || reason == "") {
		return "", ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var applicant, snapshot string
	if err = tx.QueryRowContext(ctx, `SELECT submitted_by,contract_snapshot_id FROM settlement_invoice_request WHERE id=? AND tenant_id=? AND status='SPECIAL_REVIEW' AND version=? FOR UPDATE`, id, p.TenantID, version).Scan(&applicant, &snapshot); err != nil {
		return "", ErrConflict
	}
	if applicant == p.UserID {
		return "", ErrForbidden
	}
	if err = s.RequireContractAccessTx(ctx, tx, p, "settlement.invoice.special_approve", snapshot); err != nil {
		return "", err
	}
	state := "APPROVED"
	if !approve {
		state = "REJECTED"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_special_review SET status=?,reviewed_by=?,review_reason=? WHERE tenant_id=? AND invoice_request_id=? AND required_review=1`, state, p.UserID, reason, p.TenantID, id); err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_SPECIAL_REVIEW_"+state, "invoice_request", id, map[string]any{"actor_id": p.UserID, "reason": reason}); err != nil {
		return "", err
	}
	if approve {
		return s.createApprovedInvoiceAttempt(ctx, tx, p, id, version)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='REJECTED',rejection_reason=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, reason, id, p.TenantID); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request_allocation SET released_amount=reserved_amount,status='RELEASED' WHERE tenant_id=? AND invoice_request_id=? AND status='RESERVED'`, p.TenantID, id); err != nil {
		return "", err
	}
	return "", tx.Commit()
}
