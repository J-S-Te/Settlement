package service

import (
	"context"
	"database/sql"
)

func archiveInvoiceDocumentTx(ctx context.Context, tx *sql.Tx, p Principal, invoiceID, documentType, originalName, mediaType, objectID, checksum string) (string, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_tax_invoice WHERE id=? AND tenant_id=?`, invoiceID, p.TenantID).Scan(&exists); err != nil {
		return "", err
	}
	if exists == 0 {
		return "", ErrNotFound
	}
	id := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO settlement_invoice_document(id,tenant_id,tax_invoice_id,document_type,original_name,media_type,storage_object_id,checksum,access_classification,archived_at) VALUES(?,?,?,?,?,?,?,?, 'INTERNAL',UTC_TIMESTAMP(3))`, id, p.TenantID, invoiceID, documentType, originalName, mediaType, objectID, checksum); err != nil {
		return "", err
	}
	if err := outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_DOCUMENT_ARCHIVED", "tax_invoice", invoiceID, map[string]any{"actor_id": p.UserID, "document_id": id, "document_type": documentType, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return id, nil
}

// RequestInvoiceRedFlush 记录受控请求。财务审核是
// 独立步骤，只有已审批请求才能发出税控适配器命令。
