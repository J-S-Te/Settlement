package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (s *Service) RegisterManualInvoice(ctx context.Context, p Principal, requestID string, in ManualInvoiceInput) (string, error) {
	code, err := required(in.InvoiceCode, "invoice_code")
	if err != nil {
		return "", err
	}
	no, err := required(in.InvoiceNo, "invoice_no")
	if err != nil {
		return "", err
	}
	// 票号与开票日期都来自客户端人工登记：长度/字符白名单、北京业务日上界
	// 必须服务端判定，不能只靠前端表单。
	if err = validateInvoiceDocumentNumber(code, "invoice_code"); err != nil {
		return "", err
	}
	if err = validateInvoiceDocumentNumber(no, "invoice_no"); err != nil {
		return "", err
	}
	issueDate, err := validateInvoiceIssueDate(in.IssueDate)
	if err != nil {
		return "", err
	}
	// 销售方档案同样按白名单受控，拒绝任意键写入票面快照。
	sellerProfile, err := normalizeInvoiceProfile(in.SellerProfile, sellerProfileFields, "seller_profile")
	if err != nil {
		return "", err
	}
	seller, _ := json.Marshal(sellerProfile)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var kind, currency, excl, tax, incl string
	var buyer []byte
	if err = tx.QueryRowContext(ctx, `SELECT ir.invoice_type,cs.currency,ir.amount_excl_tax,ir.tax_amount,ir.amount_incl_tax,ir.buyer_profile_snapshot FROM settlement_invoice_request ir JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id JOIN settlement_invoice_issue_attempt ia ON ia.invoice_request_id=ir.id AND ia.tenant_id=ir.tenant_id AND ia.channel='MANUAL' AND ia.status='PENDING' WHERE ir.id=? AND ir.tenant_id=? AND ir.status='ISSUE_PENDING' FOR UPDATE`, requestID, p.TenantID).Scan(&kind, &currency, &excl, &tax, &incl, &buyer); err != nil {
		return "", ErrConflict
	}
	// 人工开票按ID直接写入：合同范围从申请行解析，范围外不得关单和占用可开额度。
	if err = s.requireInvoiceRequestScopeTx(ctx, tx, p, "settlement.invoice.issue", requestID); err != nil {
		return "", err
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if len(currency) != 3 {
		return "", fmt.Errorf("%w: invoice currency is missing", ErrInvalid)
	}
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_tax_invoice(id,tenant_id,invoice_request_id,invoice_code,invoice_no,invoice_type,currency,amount_excl_tax,tax_amount,amount_incl_tax,issue_date,buyer_profile_snapshot,seller_profile_snapshot,status,issued_by_channel,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'ISSUED','MANUAL',UTC_TIMESTAMP(3))`, id, p.TenantID, requestID, code, no, kind, currency, excl, tax, incl, issueDate, buyer, seller)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='ISSUED',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, requestID, p.TenantID)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET status='ISSUED',external_invoice_id=?,responded_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND invoice_request_id=? AND channel='MANUAL' AND status='PENDING'`, id, p.TenantID, requestID)
	if err != nil {
		return "", err
	}
	rows, err := tx.QueryContext(ctx, `SELECT receivable_id,reserved_amount FROM settlement_invoice_request_allocation WHERE tenant_id=? AND invoice_request_id=? AND status='RESERVED' FOR UPDATE`, p.TenantID, requestID)
	if err != nil {
		return "", err
	}
	type allocation struct{ receivableID, amount string }
	allocations := []allocation{}
	for rows.Next() {
		var item allocation
		if err = rows.Scan(&item.receivableID, &item.amount); err != nil {
			rows.Close()
			return "", err
		}
		allocations = append(allocations, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	for _, item := range allocations {
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request_allocation SET invoiced_amount=reserved_amount,status='INVOICED' WHERE tenant_id=? AND invoice_request_id=? AND receivable_id=?`, p.TenantID, requestID, item.receivableID); err != nil {
			return "", err
		}
		if err = incrementInvoiced(ctx, tx, p.TenantID, item.receivableID, item.amount); err != nil {
			return "", err
		}
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_ISSUED", "tax_invoice", id, map[string]any{"actor_id": p.UserID, "invoice_request_id": requestID, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

type InvoiceRedFlushInput struct {
	ReasonCode   string `json:"reason_code"`
	ReasonDetail string `json:"reason_detail"`
}
