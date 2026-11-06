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
	id := newID()
	// 红冲申请的幂等键绑定原票与原因：同 key 改原因不得回放旧成功。
	idempotencyRequest := struct {
		InvoiceID    string `json:"invoice_id"`
		ReasonCode   string `json:"reason_code"`
		ReasonDetail string `json:"reason_detail"`
	}{invoiceID, reasonCode, reason}
	if existing, replay, claimErr := claimIdempotency(ctx, tx, p.TenantID, "REQUEST_INVOICE_RED_FLUSH", key, id, idempotencyRequest); claimErr != nil {
		return "", claimErr
	} else if replay {
		return existing, nil
	}
	var invoiceStatus, snapshotID string
	if err = tx.QueryRowContext(ctx, `SELECT ti.status,ir.contract_snapshot_id FROM settlement_tax_invoice ti JOIN settlement_invoice_request ir ON ir.id=ti.invoice_request_id AND ir.tenant_id=ti.tenant_id WHERE ti.id=? AND ti.tenant_id=? FOR UPDATE`, invoiceID, p.TenantID).Scan(&invoiceStatus, &snapshotID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	// 合同范围只取单据关系解析出的快照，范围外按不存在关闭，先拒绝后写。
	if err = s.requireWriteScopeTx(ctx, tx, p, "settlement.invoice.issue", snapshotID); err != nil {
		return "", err
	}
	if invoiceStatus != "ISSUED" {
		return "", fmt.Errorf("%w: invoice is not eligible for red flush", ErrConflict)
	}
	// ISSUE_UNKNOWN 仍是未收敛的在途结果：原票尚未确定是否已被税控红冲，
	// 必须计入在途白名单，否则同一原票可被创建第二笔红冲并重复冲减。
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_invoice_red_flush_request WHERE tenant_id=? AND original_invoice_id=? AND status IN ('SUBMITTED','ISSUE_PENDING','PROCESSING','ISSUE_UNKNOWN')`, p.TenantID, invoiceID).Scan(&active); err != nil {
		return "", err
	}
	if active > 0 {
		return "", fmt.Errorf("%w: active red flush request already exists", ErrConflict)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_red_flush_request(id,tenant_id,original_invoice_id,reason_code,reason_detail,idempotency_key,status,requested_by,created_at,updated_at) VALUES(?,?,?,?,?,?,'SUBMITTED',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, invoiceID, reasonCode, reason, key, p.UserID); err != nil {
		if mysqlDuplicate(err) {
			// 幂等记录不存在而红冲表已占用同 key：历史行无法核对内容，
			// 按冲突返回，不伪装成首次成功。
			return "", fmt.Errorf("%w: idempotency key was used before request binding", ErrConflict)
		}
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
