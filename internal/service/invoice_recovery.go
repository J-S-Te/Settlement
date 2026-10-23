package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// RecoverFailedInvoice 将已明确失败的请求返回审批
// 队列，不提交另一条税控命令，必须另行审批。
func (s *Service) RecoverFailedInvoice(ctx context.Context, p Principal, key, id string, version int, reason string) error {
	reason = strings.TrimSpace(reason)
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 128 || p.UserID == "" || p.TenantID == "" || version < 1 || len([]rune(reason)) < 5 || len([]rune(reason)) > 1000 {
		return fmt.Errorf("%w: 复核原因应为 5 到 1000 字，且必须提供当前版本", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, replay, err := claimIdempotency(ctx, tx, p.TenantID, "RECOVER_FAILED_INVOICE", key, id, map[string]any{"actor_id": p.UserID, "id": id, "version": version, "reason": reason})
	if err != nil {
		return err
	}
	if replay {
		return tx.Commit()
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
	if status != "ISSUE_FAILED" || currentVersion != version {
		return ErrConflict
	}
	if applicant == p.UserID {
		return fmt.Errorf("%w: 申请人不能复核自己的开票失败", ErrInvalid)
	}
	var attemptID, attemptStatus, channel, provider string
	var resultEvent sql.NullString
	var sequence uint64
	err = tx.QueryRowContext(ctx, `SELECT id,status,channel,result_sequence,last_result_event_id,provider_code FROM settlement_invoice_issue_attempt WHERE invoice_request_id=? AND tenant_id=? ORDER BY attempt_no DESC,requested_at DESC,id DESC LIMIT 1 FOR UPDATE`, id, p.TenantID).Scan(&attemptID, &attemptStatus, &channel, &sequence, &resultEvent, &provider)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if !recoverableInvoiceFailure(attemptStatus, channel, sequence, resultEvent.String) || s.invoiceIssuanceMode() != "tax_adapter" || provider != s.taxProviderCode() {
		return ErrConflict
	}
	var invoices int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_tax_invoice WHERE invoice_request_id=? AND tenant_id=?`, id, p.TenantID).Scan(&invoices); err != nil {
		return err
	}
	if invoices != 0 {
		return ErrConflict
	}
	var conflicts int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_tax_result_inbox WHERE tenant_id=? AND operation_type='INVOICE_ISSUE' AND aggregate_id=? AND processing_status='PROVIDER_CONFLICT'`, p.TenantID, attemptID).Scan(&conflicts); err != nil {
		return err
	}
	if conflicts != 0 {
		return fmt.Errorf("%w: 税控结果存在冲突，禁止重新开票", ErrConflict)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='SUBMITTED',approved_by='',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND version=? AND status='ISSUE_FAILED'`, id, p.TenantID, version); err != nil {
		return err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_FAILURE_REVIEWED", "invoice_request", id, map[string]any{"actor_id": p.UserID, "failed_attempt_id": attemptID, "reason": reason, "previous_version": version, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return err
	}
	return tx.Commit()
}

func recoverableInvoiceFailure(status, channel string, sequence uint64, eventID string) bool {
	return status == "ISSUE_FAILED" && channel == "TAX_ADAPTER" && sequence > 0 && strings.TrimSpace(eventID) != ""
}
