package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func (s *Service) ReviewInvoiceRedFlush(ctx context.Context, p Principal, id string, approve bool, in InvoiceRedFlushReviewInput) error {
	if in.Version < 1 {
		return fmt.Errorf("%w: version is required", ErrInvalid)
	}
	reason := strings.TrimSpace(in.Reason)
	if !approve && reason == "" {
		return fmt.Errorf("%w: rejection reason is required", ErrInvalid)
	}
	if len(reason) > 500 {
		return fmt.Errorf("%w: reason is too long", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var invoiceID, reasonCode, reasonDetail, requestedBy, reviewedBy, reviewReason, status string
	var version int
	err = tx.QueryRowContext(ctx, `SELECT original_invoice_id,reason_code,reason_detail,requested_by,reviewed_by,review_reason,status,version FROM settlement_invoice_red_flush_request WHERE id=? AND tenant_id=? FOR UPDATE`, id, p.TenantID).Scan(&invoiceID, &reasonCode, &reasonDetail, &requestedBy, &reviewedBy, &reviewReason, &status, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if approve && status == "ISSUE_PENDING" && reviewedBy == p.UserID {
		return tx.Commit()
	}
	if !approve && status == "REJECTED" && reviewedBy == p.UserID && reviewReason == reason {
		return tx.Commit()
	}
	if status != "SUBMITTED" || version != in.Version {
		return ErrConflict
	}
	if requestedBy == p.UserID {
		return fmt.Errorf("%w: requester cannot review own red flush request", ErrConflict)
	}
	if approve {
		if s.invoiceIssuanceMode() != "tax_adapter" {
			return fmt.Errorf("%w: red flush approval requires tax_adapter issuance mode", ErrConflict)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_request SET status='ISSUE_PENDING',reviewed_by=?,review_reason='',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status='SUBMITTED' AND version=?`, p.UserID, id, p.TenantID, in.Version); err != nil {
			return err
		}
		attemptID := newID()
		if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_red_flush_attempt(id,tenant_id,red_flush_request_id,original_invoice_id,attempt_no,provider_code,external_request_id,status,requested_at) VALUES(?,?,?,?,1,?,?,'PENDING',UTC_TIMESTAMP(3))`, attemptID, p.TenantID, id, invoiceID, s.taxProviderCode(), attemptID); err != nil {
			return err
		}
		var invoiceCode, invoiceNo, invoiceType, currency, excl, tax, incl, providerCode string
		var externalInvoiceID sql.NullString
		if err = tx.QueryRowContext(ctx, `SELECT invoice_code,invoice_no,invoice_type,currency,amount_excl_tax,tax_amount,amount_incl_tax,provider_code,external_invoice_id FROM settlement_tax_invoice WHERE id=? AND tenant_id=?`, invoiceID, p.TenantID).Scan(&invoiceCode, &invoiceNo, &invoiceType, &currency, &excl, &tax, &incl, &providerCode, &externalInvoiceID); err != nil {
			return err
		}
		command := map[string]any{"operation_type": "RED_FLUSH", "provider_code": s.taxProviderCode(), "external_request_id": attemptID, "red_flush_attempt_id": attemptID, "red_flush_request_id": id, "original_invoice_id": invoiceID, "original_provider_code": providerCode, "original_external_invoice_id": externalInvoiceID.String, "invoice_code": invoiceCode, "invoice_no": invoiceNo, "invoice_type": invoiceType, "currency": currency, "amount_excl_tax": excl, "tax_amount": tax, "amount_incl_tax": incl, "reason_code": reasonCode, "reason_detail": reasonDetail}
		if err = outbox(ctx, tx, p.TenantID, "TAX_INVOICE_COMMAND", "SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED", "tax_invoice", invoiceID, command); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_attempt SET command_event_id=(SELECT event_id FROM settlement_outbox_event WHERE tenant_id=? AND destination='TAX_INVOICE_COMMAND' AND event_type='SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED' AND aggregate_id=? ORDER BY created_at DESC,id DESC LIMIT 1) WHERE id=? AND tenant_id=?`, p.TenantID, invoiceID, attemptID, p.TenantID); err != nil {
			return err
		}
		if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_RED_FLUSH_APPROVED", "invoice_red_flush_request", id, map[string]any{"actor_id": p.UserID, "invoice_id": invoiceID, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
			return err
		}
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_request SET status='REJECTED',reviewed_by=?,review_reason=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status='SUBMITTED' AND version=?`, p.UserID, reason, id, p.TenantID, in.Version); err != nil {
			return err
		}
		if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_RED_FLUSH_REJECTED", "invoice_red_flush_request", id, map[string]any{"actor_id": p.UserID, "invoice_id": invoiceID, "reason": reason, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
