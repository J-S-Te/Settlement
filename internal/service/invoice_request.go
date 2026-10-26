package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
)

const createInvoiceRequestCommand = "CREATE_INVOICE_REQUEST"

func (s *Service) CreateInvoiceRequest(ctx context.Context, p Principal, key string, in InvoiceRequestInput) (string, error) {
	if _, err := required(key, "Idempotency-Key"); err != nil {
		return "", err
	}
	if len(in.Items) == 0 || len(in.Allocations) == 0 {
		return "", fmt.Errorf("%w: items and allocations are required", ErrInvalid)
	}
	for index, item := range in.Items {
		if textTooLong(item.ItemName, 255) || textTooLong(item.TaxClassificationCode, 64) ||
			textTooLong(item.Specification, 128) || textTooLong(item.Unit, 32) {
			return "", fmt.Errorf("%w: item %d text exceeds the length limit", ErrInvalid, index+1)
		}
	}
	buyer, _ := json.Marshal(in.BuyerProfile)
	normalized, excl, tax, incl, err := normalizeInvoiceItems(in.Items)
	if err != nil {
		return "", err
	}
	allocationTotal := new(big.Rat)
	for i := range in.Allocations {
		reserved, err := amount(in.Allocations[i].ReservedAmount)
		if err != nil {
			return "", err
		}
		in.Allocations[i].ReservedAmount = reserved
		allocationTotal.Add(allocationTotal, add(reserved))
	}
	if allocationTotal.Cmp(incl) != 0 {
		return "", fmt.Errorf("%w: allocation total must equal invoice total", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id := newID()
	idempotencyRequest := struct {
		ContractSnapshotID string                   `json:"contract_snapshot_id"`
		BuyerProfile       json.RawMessage          `json:"buyer_profile"`
		InvoiceType        string                   `json:"invoice_type"`
		Items              []InvoiceItemInput       `json:"items"`
		Allocations        []InvoiceAllocationInput `json:"allocations"`
	}{in.ContractSnapshotID, buyer, in.InvoiceType, normalized, in.Allocations}
	if existing, replay, claimErr := claimIdempotency(ctx, tx, p.TenantID, createInvoiceRequestCommand, key, id, idempotencyRequest); claimErr != nil {
		return "", claimErr
	} else if replay {
		return existing, nil
	}
	var snapshot string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_contract_snapshot WHERE id=? AND tenant_id=? AND financial_status='EFFECTIVE'`, in.ContractSnapshotID, p.TenantID).Scan(&snapshot); err != nil {
		return "", ErrInvalid
	}
	for _, allocation := range in.Allocations {
		reserved := allocation.ReservedAmount
		var available string
		err = tx.QueryRowContext(ctx, `SELECT original_amount-invoiced_amount-COALESCE((SELECT SUM(reserved_amount-invoiced_amount-released_amount) FROM settlement_invoice_request_allocation a WHERE a.tenant_id=r.tenant_id AND a.receivable_id=r.id AND a.status='RESERVED'),0) FROM settlement_receivable r WHERE r.id=? AND r.tenant_id=? AND r.contract_snapshot_id=? AND r.recognition_status='CONFIRMED' FOR UPDATE`, allocation.ReceivableID, p.TenantID, in.ContractSnapshotID).Scan(&available)
		if err != nil || decimalGreater(reserved, available) {
			return "", ErrConflict
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_request(id,tenant_id,request_no,contract_snapshot_id,buyer_profile_snapshot,invoice_type,amount_excl_tax,tax_amount,amount_incl_tax,status,idempotency_key,submitted_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'SUBMITTED',?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, "IR-"+id[:12], in.ContractSnapshotID, buyer, in.InvoiceType, excl.FloatString(2), tax.FloatString(2), incl.FloatString(2), key, p.UserID)
	if err != nil {
		if mysqlDuplicate(err) {
			var existing string
			if e := tx.QueryRowContext(ctx, `SELECT id FROM settlement_invoice_request WHERE tenant_id=? AND idempotency_key=?`, p.TenantID, key).Scan(&existing); e == nil {
				return existing, nil
			}
		}
		return "", err
	}
	for i, item := range normalized {
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_request_item(id,tenant_id,invoice_request_id,line_no,item_name,tax_classification_code,specification,unit,quantity,unit_price_excl_tax,amount_excl_tax,tax_rate,tax_amount,amount_incl_tax) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, newID(), p.TenantID, id, i+1, item.ItemName, item.TaxClassificationCode, item.Specification, item.Unit, item.Quantity, item.UnitPriceExclTax, item.AmountExclTax, item.TaxRate, item.TaxAmount, item.AmountInclTax)
		if err != nil {
			return "", err
		}
	}
	for _, allocation := range in.Allocations {
		reserved, _ := amount(allocation.ReservedAmount)
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_request_allocation(id,tenant_id,invoice_request_id,receivable_id,reserved_amount,status) VALUES(?,?,?,?,?,'RESERVED')`, newID(), p.TenantID, id, allocation.ReceivableID, reserved)
		if err != nil {
			return "", err
		}
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_REQUEST_SUBMITTED", "invoice_request", id, map[string]any{"actor_id": p.UserID, "request_id": id, "result": "SUCCESS", "risk_level": "MEDIUM"}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}
