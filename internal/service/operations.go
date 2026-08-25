package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

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
	ContractSnapshotID string                   `json:"contract_snapshot_id"`
	BuyerProfile       map[string]any           `json:"buyer_profile"`
	InvoiceType        string                   `json:"invoice_type"`
	Items              []InvoiceItemInput       `json:"items"`
	Allocations        []InvoiceAllocationInput `json:"allocations"`
}

func decimal(value string, positive bool) (string, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(value))
	if !ok || r.Sign() < 0 || (positive && r.Sign() == 0) || r.Denom().Cmp(big.NewInt(1_000_000)) > 0 {
		return "", fmt.Errorf("%w: invalid decimal", ErrInvalid)
	}
	return r.FloatString(6), nil
}
func add(values ...string) *big.Rat {
	sum := new(big.Rat)
	for _, value := range values {
		r, _ := new(big.Rat).SetString(value)
		sum.Add(sum, r)
	}
	return sum
}
func outbox(ctx context.Context, tx *sql.Tx, tenant, destination, eventType, aggregateType, aggregateID string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_outbox_event(id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,status,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?, 'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, id, destination, eventType, aggregateType, aggregateID, body)
	if err != nil {
		return err
	}
	if destination == "PLATFORM_AUDIT" {
		actor := "SYSTEM"
		if value, ok := payload.(map[string]any); ok {
			if v, ok := value["actor_id"].(string); ok && v != "" {
				actor = v
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_audit_event(id,tenant_id,actor_id,action,resource_type,resource_id,result,reason_code,request_id,correlation_id,detail_json,occurred_at) VALUES(?,?,?,?,?,?,'SUCCESS','',?,?,?,UTC_TIMESTAMP(3))`, newID(), tenant, actor, eventType, aggregateType, aggregateID, id, id, body)
	}
	return err
}

func (s *Service) CreateInvoiceRequest(ctx context.Context, p Principal, key string, in InvoiceRequestInput) (string, error) {
	if _, err := required(key, "Idempotency-Key"); err != nil {
		return "", err
	}
	if len(in.Items) == 0 || len(in.Allocations) == 0 {
		return "", fmt.Errorf("%w: items and allocations are required", ErrInvalid)
	}
	buyer, _ := json.Marshal(in.BuyerProfile)
	var excl, tax, incl *big.Rat = new(big.Rat), new(big.Rat), new(big.Rat)
	normalized := make([]InvoiceItemInput, 0, len(in.Items))
	for _, item := range in.Items {
		q, err := decimal(item.Quantity, true)
		if err != nil {
			return "", err
		}
		price, err := decimal(item.UnitPriceExclTax, false)
		if err != nil {
			return "", err
		}
		ae, err := amount(item.AmountExclTax)
		if err != nil {
			return "", err
		}
		tr, err := decimal(item.TaxRate, false)
		if err != nil {
			return "", err
		}
		ta, err := decimal(item.TaxAmount, false)
		if err != nil {
			return "", err
		}
		ai, err := amount(item.AmountInclTax)
		if err != nil {
			return "", err
		}
		if add(ae, ta).Cmp(add(ai)) != 0 {
			return "", fmt.Errorf("%w: invoice line total mismatch", ErrInvalid)
		}
		item.Quantity = q
		item.UnitPriceExclTax = price
		item.AmountExclTax = ae
		item.TaxRate = tr
		item.TaxAmount = ta
		item.AmountInclTax = ai
		normalized = append(normalized, item)
		excl.Add(excl, add(ae))
		tax.Add(tax, add(ta))
		incl.Add(incl, add(ai))
	}
	allocationTotal := new(big.Rat)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var snapshot string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_contract_snapshot WHERE id=? AND tenant_id=? AND financial_status='EFFECTIVE'`, in.ContractSnapshotID, p.TenantID).Scan(&snapshot); err != nil {
		return "", ErrInvalid
	}
	id := newID()
	for _, allocation := range in.Allocations {
		reserved, err := amount(allocation.ReservedAmount)
		if err != nil {
			return "", err
		}
		var available string
		err = tx.QueryRowContext(ctx, `SELECT original_amount-invoiced_amount-COALESCE((SELECT SUM(reserved_amount-invoiced_amount-released_amount) FROM settlement_invoice_request_allocation a WHERE a.receivable_id=r.id AND a.status='RESERVED'),0) FROM settlement_receivable r WHERE r.id=? AND r.tenant_id=? AND r.contract_snapshot_id=? AND r.recognition_status='CONFIRMED' FOR UPDATE`, allocation.ReceivableID, p.TenantID, in.ContractSnapshotID).Scan(&available)
		if err != nil || decimalGreater(reserved, available) {
			return "", ErrConflict
		}
		allocation.ReservedAmount = reserved
		allocationTotal.Add(allocationTotal, add(reserved))
	}
	if allocationTotal.Cmp(incl) != 0 {
		return "", fmt.Errorf("%w: allocation total must equal invoice total", ErrInvalid)
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
	attempt := newID()
	_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='ISSUE_PENDING',approved_by=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND version=?`, p.UserID, id, p.TenantID, version)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_issue_attempt(id,tenant_id,invoice_request_id,attempt_no,channel,external_request_id,idempotency_key,status,requested_at) VALUES(?,?,?,?,?,?,?,'PENDING',UTC_TIMESTAMP(3))`, attempt, p.TenantID, id, 1, "TAX_ADAPTER", attempt, attempt)
	if err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "TAX_INVOICE_COMMAND", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "invoice_request", id, map[string]any{"invoice_request_id": id, "attempt_id": attempt}); err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_APPROVED", "invoice_request", id, map[string]any{"actor_id": p.UserID, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return attempt, tx.Commit()
}

type ManualInvoiceInput struct {
	InvoiceCode   string         `json:"invoice_code"`
	InvoiceNo     string         `json:"invoice_no"`
	IssueDate     string         `json:"issue_date"`
	SellerProfile map[string]any `json:"seller_profile"`
}

// RegisterManualInvoice is the safe first-stage adapter: approved requests can
// be closed without coupling the core ledger to a particular tax vendor.
func (s *Service) RegisterManualInvoice(ctx context.Context, p Principal, requestID string, in ManualInvoiceInput) (string, error) {
	code, err := required(in.InvoiceCode, "invoice_code")
	if err != nil {
		return "", err
	}
	no, err := required(in.InvoiceNo, "invoice_no")
	if err != nil {
		return "", err
	}
	if _, err = time.Parse("2006-01-02", in.IssueDate); err != nil {
		return "", ErrInvalid
	}
	seller, _ := json.Marshal(in.SellerProfile)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var kind, excl, tax, incl string
	var buyer []byte
	if err = tx.QueryRowContext(ctx, `SELECT invoice_type,amount_excl_tax,tax_amount,amount_incl_tax,buyer_profile_snapshot FROM settlement_invoice_request WHERE id=? AND tenant_id=? AND status='ISSUE_PENDING' FOR UPDATE`, requestID, p.TenantID).Scan(&kind, &excl, &tax, &incl, &buyer); err != nil {
		return "", ErrConflict
	}
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_tax_invoice(id,tenant_id,invoice_request_id,invoice_code,invoice_no,invoice_type,amount_excl_tax,tax_amount,amount_incl_tax,issue_date,buyer_profile_snapshot,seller_profile_snapshot,status,issued_by_channel,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?, 'ISSUED','MANUAL',UTC_TIMESTAMP(3))`, id, p.TenantID, requestID, code, no, kind, excl, tax, incl, in.IssueDate, buyer, seller)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='ISSUED',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, requestID, p.TenantID)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET status='ISSUED',external_invoice_id=?,responded_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND invoice_request_id=? AND status='PENDING'`, id, p.TenantID, requestID)
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
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET invoiced_amount=invoiced_amount+?,invoice_status=CASE WHEN invoiced_amount+? >= original_amount THEN 'FULLY_INVOICED' ELSE 'PARTIALLY_INVOICED' END,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, item.amount, item.amount, item.receivableID, p.TenantID); err != nil {
			return "", err
		}
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_ISSUED", "tax_invoice", id, map[string]any{"actor_id": p.UserID, "invoice_request_id": requestID, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

type ReversalInput struct {
	Amount       string `json:"amount"`
	ReasonCode   string `json:"reason_code"`
	ReasonDetail string `json:"reason_detail"`
}

func (s *Service) ReverseAllocation(ctx context.Context, p Principal, key, allocationID string, in ReversalInput) (string, error) {
	value, err := amount(in.Amount)
	if err != nil {
		return "", err
	}
	if _, err = required(key, "Idempotency-Key"); err != nil {
		return "", err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var receiptID, receivableID, currency, confirmedBy, allocated, reversed string
	err = tx.QueryRowContext(ctx, `SELECT a.receipt_id,a.receivable_id,a.currency,a.confirmed_by,a.allocated_amount,COALESCE((SELECT SUM(r.reversed_amount) FROM settlement_receipt_allocation_reversal r WHERE r.original_allocation_id=a.id),0) FROM settlement_receipt_allocation a WHERE a.id=? AND a.tenant_id=? AND a.status IN('CONFIRMED','PARTIALLY_REVERSED') FOR UPDATE`, allocationID, p.TenantID).Scan(&receiptID, &receivableID, &currency, &confirmedBy, &allocated, &reversed)
	if err != nil {
		return "", ErrConflict
	}
	if confirmedBy == p.UserID {
		return "", fmt.Errorf("%w: allocation confirmer cannot approve reversal", ErrInvalid)
	}
	remaining := new(big.Rat).Sub(add(allocated), add(reversed))
	if decimalGreater(value, remaining.FloatString(2)) {
		return "", ErrConflict
	}
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receipt_allocation_reversal(id,tenant_id,reversal_no,original_allocation_id,reversed_amount,reason_code,reason_detail,approved_by,reversed_at,idempotency_key) VALUES(?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),?)`, id, p.TenantID, "RV-"+id[:12], allocationID, value, in.ReasonCode, in.ReasonDetail, p.UserID, key)
	if err != nil {
		if mysqlDuplicate(err) {
			var existing string
			if e := tx.QueryRowContext(ctx, `SELECT id FROM settlement_receipt_allocation_reversal WHERE tenant_id=? AND idempotency_key=?`, p.TenantID, key).Scan(&existing); e == nil {
				return existing, nil
			}
		}
		return "", err
	}
	full := remaining.Cmp(add(value)) == 0
	status := "PARTIALLY_REVERSED"
	if full {
		status = "REVERSED"
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receipt_allocation SET status=? WHERE id=? AND tenant_id=?`, status, allocationID, p.TenantID)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receipt SET unallocated_amount=unallocated_amount+?,status=CASE WHEN unallocated_amount+?=amount THEN 'AVAILABLE' ELSE 'PARTIALLY_ALLOCATED' END,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, value, value, receiptID, p.TenantID)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET received_allocated_amount=received_allocated_amount-?,open_amount=open_amount+?,collection_status=CASE WHEN received_allocated_amount-?=0 THEN 'UNPAID' ELSE 'PARTIALLY_SETTLED' END,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, value, value, value, receivableID, p.TenantID)
	if err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_ALLOCATION_REVERSED", "receipt_allocation", allocationID, map[string]any{"actor_id": p.UserID, "reversal_id": id, "reason_code": in.ReasonCode, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

type DunningPolicyInput struct {
	Name               string `json:"name"`
	AgingFromDays      int    `json:"aging_from_days"`
	AgingToDays        int    `json:"aging_to_days"`
	ActionType         string `json:"action_type"`
	RecipientRule      string `json:"recipient_rule"`
	Channel            string `json:"channel"`
	RepeatIntervalDays int    `json:"repeat_interval_days"`
	Priority           string `json:"priority"`
}

func (s *Service) CreateDunningPolicy(ctx context.Context, p Principal, in DunningPolicyInput) (string, error) {
	if in.AgingFromDays < 0 || in.AgingToDays < in.AgingFromDays || in.RepeatIntervalDays < 1 || !strings.HasPrefix(in.RecipientRule, "USER:") || (in.Priority != "NORMAL" && in.Priority != "HIGH" && in.Priority != "CRITICAL") {
		return "", ErrInvalid
	}
	if in.Channel != "LOCAL" && in.Channel != "PLATFORM" {
		return "", ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_dunning_policy(id,tenant_id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled,created_at,updated_at) VALUES(?,?,?,1,?,?,?,?,?,?,?,TRUE,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, in.Name, in.AgingFromDays, in.AgingToDays, in.ActionType, in.RecipientRule, in.Channel, in.RepeatIntervalDays, in.Priority)
	if err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_DUNNING_POLICY_CREATED", "dunning_policy", id, map[string]any{"actor_id": p.UserID, "result": "SUCCESS", "risk_level": "MEDIUM"}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}
