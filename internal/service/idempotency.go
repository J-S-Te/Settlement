package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
)

func claimIdempotency(ctx context.Context, tx *sql.Tx, tenant, command, key, resourceID string, request any) (string, bool, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return "", false, err
	}
	digest := sha256.Sum256(body)
	response, _ := json.Marshal(map[string]string{"id": resourceID})
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_idempotency_record(tenant_id,command_type,idempotency_key,request_hash,resource_id,response_json,created_at) VALUES(?,?,?,?,?,?,UTC_TIMESTAMP(3))`, tenant, command, key, digest[:], resourceID, response)
	if err == nil {
		return resourceID, false, nil
	}
	if !mysqlDuplicate(err) {
		return "", false, err
	}
	var storedHash []byte
	var storedID string
	if err = tx.QueryRowContext(ctx, `SELECT request_hash,resource_id FROM settlement_idempotency_record WHERE tenant_id=? AND command_type=? AND idempotency_key=?`, tenant, command, key).Scan(&storedHash, &storedID); err != nil {
		return "", false, err
	}
	if !bytes.Equal(storedHash, digest[:]) {
		return "", false, fmt.Errorf("%w: idempotency key was used with a different request", ErrConflict)
	}
	return storedID, true, nil
}

type InvoiceItemInput struct {
	ItemName              string `json:"item_name"`
	TaxClassificationCode string `json:"tax_classification_code"`
	Specification         string `json:"specification"`
	Unit                  string `json:"unit"`
	Quantity              string `json:"quantity"`
	UnitPriceExclTax      string `json:"unit_price_excl_tax"`
	AmountExclTax         string `json:"amount_excl_tax"`
	TaxRate               string `json:"tax_rate"`
	TaxAmount             string `json:"tax_amount"`
	AmountInclTax         string `json:"amount_incl_tax"`
}
type InvoiceAllocationInput struct {
	ReceivableID   string `json:"receivable_id"`
	ReservedAmount string `json:"reserved_amount"`
}
type InvoiceRequestInput struct {
	SpecialReason      string                   `json:"special_reason"`
	ContractSnapshotID string                   `json:"contract_snapshot_id"`
	BuyerProfile       map[string]any           `json:"buyer_profile"`
	InvoiceType        string                   `json:"invoice_type"`
	Items              []InvoiceItemInput       `json:"items"`
	Allocations        []InvoiceAllocationInput `json:"allocations"`
}
