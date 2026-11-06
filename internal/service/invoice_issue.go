package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func invoiceIssueCommand(ctx context.Context, tx *sql.Tx, tenant, requestID, attemptID, provider string) (map[string]any, error) {
	var requestNo, invoiceType, currency, excl, tax, incl string
	var buyer []byte
	err := tx.QueryRowContext(ctx, `SELECT ir.request_no,ir.invoice_type,cs.currency,ir.amount_excl_tax,ir.tax_amount,ir.amount_incl_tax,ir.buyer_profile_snapshot FROM settlement_invoice_request ir JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id WHERE ir.id=? AND ir.tenant_id=?`, requestID, tenant).Scan(&requestNo, &invoiceType, &currency, &excl, &tax, &incl, &buyer)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT line_no,item_name,tax_classification_code,specification,unit,quantity,unit_price_excl_tax,amount_excl_tax,tax_rate,tax_amount,amount_incl_tax FROM settlement_invoice_request_item WHERE tenant_id=? AND invoice_request_id=? ORDER BY line_no`, tenant, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var line int
		var name, classCode, spec, unit, quantity, unitPrice, itemExcl, rate, itemTax, itemIncl string
		if err = rows.Scan(&line, &name, &classCode, &spec, &unit, &quantity, &unitPrice, &itemExcl, &rate, &itemTax, &itemIncl); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"line_no": line, "item_name": name, "tax_classification_code": classCode, "specification": spec, "unit": unit, "quantity": quantity, "unit_price_excl_tax": unitPrice, "amount_excl_tax": itemExcl, "tax_rate": rate, "tax_amount": itemTax, "amount_incl_tax": itemIncl})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var buyerProfile any
	if err = json.Unmarshal(buyer, &buyerProfile); err != nil {
		return nil, err
	}
	// 出站前再次受控：历史或库内被改写的 JSON 不能把任意键透传给税控。
	switch value := buyerProfile.(type) {
	case nil:
		// 旧数据可能是 JSON null：归一化为空档案，而不是把 null 透传出去。
		buyerProfile = map[string]any{}
	case map[string]any:
		normalizedProfile, profileErr := normalizeInvoiceProfile(value, buyerProfileFields, "buyer_profile")
		if profileErr != nil {
			return nil, profileErr
		}
		buyerProfile = normalizedProfile
	default:
		return nil, fmt.Errorf("%w: buyer profile snapshot is not an object", ErrInvalid)
	}
	if invoiceType, err = validateStoredInvoiceType(invoiceType); err != nil {
		return nil, err
	}
	return map[string]any{"operation_type": "INVOICE_ISSUE", "provider_code": provider, "external_request_id": attemptID, "attempt_id": attemptID, "invoice_request_id": requestID, "request_no": requestNo, "invoice_type": invoiceType, "currency": currency, "buyer_profile": buyerProfile, "amount_excl_tax": excl, "tax_amount": tax, "amount_incl_tax": incl, "items": items}, nil
}

// RejectInvoiceRequest 在同一事务内持久化审核决定，
// 并释放预留应收金额。审核人不能处理
// 自己的申请。
