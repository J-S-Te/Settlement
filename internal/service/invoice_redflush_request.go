package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func (s *Service) RequestInvoiceRedFlush(ctx context.Context, p Principal, key, invoiceID string, in InvoiceRedFlushInput) (string, error) {
	key, err := required(key, "Idempotency-Key")
	if err != nil {
		return "", err
	}
	reason, err := required(in.ReasonDetail, "reason_detail")
	if err != nil {
		return "", err
	}
	if len(reason) > 500 {
		return "", fmt.Errorf("%w: reason_detail is too long", ErrInvalid)
	}
	reasonCode := strings.TrimSpace(in.ReasonCode)
	allowed := map[string]bool{"INVOICE_ERROR": true, "CONTRACT_CHANGE": true, "RETURN_OR_DISCOUNT": true, "OTHER": true}
	if !allowed[reasonCode] {
		return "", fmt.Errorf("%w: invalid reason_code", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var existing, existingInvoiceID string
	err = tx.QueryRowContext(ctx, `SELECT id,original_invoice_id FROM settlement_invoice_red_flush_request WHERE tenant_id=? AND idempotency_key=?`, p.TenantID, key).Scan(&existing, &existingInvoiceID)
	if err == nil {
		if existingInvoiceID != invoiceID {
			return "", fmt.Errorf("%w: idempotency key belongs to another invoice", ErrConflict)
		}
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var invoiceStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM settlement_tax_invoice WHERE id=? AND tenant_id=? FOR UPDATE`, invoiceID, p.TenantID).Scan(&invoiceStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if invoiceStatus != "ISSUED" {
		return "", fmt.Errorf("%w: invoice is not eligible for red flush", ErrConflict)
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_invoice_red_flush_request WHERE tenant_id=? AND original_invoice_id=? AND status IN ('SUBMITTED','ISSUE_PENDING','PROCESSING')`, p.TenantID, invoiceID).Scan(&active); err != nil {
		return "", err
	}
	if active > 0 {
		return "", fmt.Errorf("%w: active red flush request already exists", ErrConflict)
	}
	id := newID()
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_red_flush_request(id,tenant_id,original_invoice_id,reason_code,reason_detail,idempotency_key,status,requested_by,created_at,updated_at) VALUES(?,?,?,?,?,?,'SUBMITTED',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, invoiceID, reasonCode, reason, key, p.UserID); err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_RED_FLUSH_SUBMITTED", "invoice_red_flush_request", id, map[string]any{"actor_id": p.UserID, "invoice_id": invoiceID, "reason_code": reasonCode, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

type InvoiceRedFlushReviewInput struct {
	Version int    `json:"version"`
	Reason  string `json:"reason"`
}
