package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid input")
)

type Service struct{ DB *sql.DB }
type ContractEvent struct {
	EventID    string    `json:"event_id"`
	EventType  string    `json:"event_type"`
	TenantID   string    `json:"tenant_id"`
	OccurredAt time.Time `json:"occurred_at"`
	Aggregate  struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	} `json:"aggregate"`
	Contract struct {
		ID           string        `json:"id"`
		No           string        `json:"no"`
		Version      int           `json:"version"`
		CustomerID   string        `json:"customer_id"`
		CustomerName string        `json:"customer_name"`
		ProjectID    string        `json:"project_id"`
		ProjectName  string        `json:"project_name"`
		Currency     string        `json:"currency"`
		Amount       string        `json:"amount"`
		PaymentTerms []PaymentTerm `json:"payment_terms"`
	} `json:"contract"`
}
type PaymentTerm struct {
	InstallmentNo int    `json:"installment_no"`
	DueDate       string `json:"due_date"`
	Amount        string `json:"amount"`
}
type Principal struct {
	TenantID, UserID string
	Permissions      map[string]bool
}

func (p Principal) Allows(permission string) bool {
	return p.Permissions[permission] || p.Permissions["settlement.admin"]
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.ToUpper(hex.EncodeToString(b))
}
func amount(value string) (string, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(value))
	if !ok || r.Sign() <= 0 || r.Denom().Cmp(big.NewInt(100)) > 0 {
		return "", fmt.Errorf("%w: amount must be a positive decimal with at most two fractional digits", ErrInvalid)
	}
	return r.FloatString(2), nil
}
func required(value, name string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%w: %s is required", ErrInvalid, name)
	}
	return value, nil
}

// IngestContract stores the original event before creating financial snapshots.
// Replayed event IDs are successful no-ops; a version that already exists with
// another event ID is rejected so an old contract update cannot overwrite facts.
func (s *Service) IngestContract(ctx context.Context, source string, event ContractEvent) (bool, error) {
	if _, err := required(event.EventID, "event_id"); err != nil {
		return false, err
	}
	if _, err := required(event.TenantID, "tenant_id"); err != nil {
		return false, err
	}
	if event.EventType != "contract.financial_effective.v1" {
		return false, fmt.Errorf("%w: unsupported event_type", ErrInvalid)
	}
	if event.Contract.Version < 1 || len(event.Contract.PaymentTerms) == 0 {
		return false, fmt.Errorf("%w: contract version and payment_terms are required", ErrInvalid)
	}
	for name, value := range map[string]string{"source": source, "contract.id": event.Contract.ID, "contract.no": event.Contract.No, "customer_id": event.Contract.CustomerID, "customer_name": event.Contract.CustomerName} {
		if _, err := required(value, name); err != nil {
			return false, err
		}
	}
	if event.Aggregate.ID != "" && event.Aggregate.ID != event.Contract.ID {
		return false, fmt.Errorf("%w: aggregate and contract IDs differ", ErrInvalid)
	}
	if event.Aggregate.Version != 0 && event.Aggregate.Version != event.Contract.Version {
		return false, fmt.Errorf("%w: aggregate and contract versions differ", ErrInvalid)
	}
	if len(event.Contract.Currency) != 3 {
		return false, fmt.Errorf("%w: currency must be a three-letter code", ErrInvalid)
	}
	contractAmount, err := amount(event.Contract.Amount)
	if err != nil {
		return false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var duplicateID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_inbox_event WHERE tenant_id=? AND source_application=? AND event_id=?`, event.TenantID, source, event.EventID).Scan(&duplicateID)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_contract_stream(tenant_id,source_application,source_contract_id,current_version,updated_at) VALUES(?,?,?,0,UTC_TIMESTAMP(3)) ON DUPLICATE KEY UPDATE updated_at=updated_at`, event.TenantID, source, event.Contract.ID); err != nil {
		return false, err
	}
	var currentVersion int
	if err = tx.QueryRowContext(ctx, `SELECT current_version FROM settlement_contract_stream WHERE tenant_id=? AND source_application=? AND source_contract_id=? FOR UPDATE`, event.TenantID, source, event.Contract.ID).Scan(&currentVersion); err != nil {
		return false, err
	}
	if event.Contract.Version <= currentVersion {
		return false, ErrConflict
	}
	inboxID := newID()
	payload, _ := json.Marshal(event)
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_inbox_event(id,tenant_id,source_application,event_id,event_type,aggregate_id,aggregate_version,payload_json,status,received_at) VALUES(?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3))`, inboxID, event.TenantID, source, event.EventID, event.EventType, event.Contract.ID, event.Contract.Version, payload, "ACCEPTED")
	if err != nil {
		if mysqlDuplicate(err) {
			return false, nil
		}
		return false, err
	}
	contractID := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,project_id,project_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, contractID, event.TenantID, source, event.EventID, event.Contract.ID, event.Contract.No, event.Contract.Version, event.Contract.CustomerID, event.Contract.CustomerName, event.Contract.ProjectID, event.Contract.ProjectName, strings.ToUpper(event.Contract.Currency), contractAmount, "EFFECTIVE", event.OccurredAt)
	if err != nil {
		if mysqlDuplicate(err) {
			return false, ErrConflict
		}
		return false, err
	}
	termTotal := new(big.Rat)
	for _, term := range event.Contract.PaymentTerms {
		a, err := amount(term.Amount)
		if err != nil {
			return false, err
		}
		if term.InstallmentNo < 1 {
			return false, fmt.Errorf("%w: installment_no must be positive", ErrInvalid)
		}
		part, _ := new(big.Rat).SetString(a)
		termTotal.Add(termTotal, part)
		if _, err := time.Parse("2006-01-02", term.DueDate); err != nil {
			return false, fmt.Errorf("%w: due_date must be YYYY-MM-DD", ErrInvalid)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,created_at,updated_at) VALUES(?,?,?,?,?,?,?, ?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newID(), event.TenantID, contractID, term.InstallmentNo, term.DueDate, a, "CONTRACT_EVENT", "PENDING_CONFIRMATION")
		if err != nil {
			return false, err
		}
	}
	expected, _ := new(big.Rat).SetString(contractAmount)
	if termTotal.Cmp(expected) != 0 {
		return false, fmt.Errorf("%w: payment term total must equal contract amount", ErrInvalid)
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_inbox_event SET status='COMPLETED',processed_at=UTC_TIMESTAMP(3) WHERE id=?`, inboxID)
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_contract_stream SET current_version=?,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND source_application=? AND source_contract_id=?`, event.Contract.Version, event.TenantID, source, event.Contract.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Service) ConfirmPlan(ctx context.Context, p Principal, id string, version int) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var snapshot, due, planned, currency string
	err = tx.QueryRowContext(ctx, `SELECT rp.contract_snapshot_id,DATE_FORMAT(rp.due_date,'%Y-%m-%d'),rp.planned_amount,cs.currency FROM settlement_receivable_plan rp JOIN settlement_contract_snapshot cs ON cs.id=rp.contract_snapshot_id WHERE rp.id=? AND rp.tenant_id=? AND rp.confirmation_status='PENDING_CONFIRMATION' AND rp.version=? FOR UPDATE`, id, p.TenantID, version).Scan(&snapshot, &due, &planned, &currency)
	if err == sql.ErrNoRows {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable_plan SET confirmation_status='CONFIRMED',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND version=?`, id, p.TenantID, version); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,receivable_plan_id,due_date,original_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newID(), p.TenantID, "AR-"+newID()[:12], snapshot, id, due, planned, planned, currency, "CONFIRMED", "UNPAID", "NOT_INVOICED")
	if err != nil {
		return err
	}
	return tx.Commit()
}

type ReceiptInput struct {
	CustomerID    string `json:"customer_id"`
	CustomerName  string `json:"customer_name"`
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
	ReceiptDate   string `json:"receipt_date"`
	PaymentMethod string `json:"payment_method"`
	BankReference string `json:"bank_transaction_reference"`
}

func (s *Service) CreateReceipt(ctx context.Context, p Principal, key string, in ReceiptInput) (string, error) {
	a, err := amount(in.Amount)
	if err != nil {
		return "", err
	}
	if _, err = required(key, "Idempotency-Key"); err != nil {
		return "", err
	}
	if _, err = time.Parse("2006-01-02", in.ReceiptDate); err != nil {
		return "", fmt.Errorf("%w: receipt_date must be YYYY-MM-DD", ErrInvalid)
	}
	id := newID()
	no := "RC-" + id[:12]
	_, err = s.DB.ExecContext(ctx, `INSERT INTO settlement_receipt(id,tenant_id,receipt_no,customer_id,customer_name_snapshot,amount,unallocated_amount,currency,receipt_date,payment_method,bank_transaction_reference,source_type,status,idempotency_key,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, no, in.CustomerID, in.CustomerName, a, a, strings.ToUpper(in.Currency), in.ReceiptDate, in.PaymentMethod, in.BankReference, "MANUAL", "AVAILABLE", key)
	if err != nil {
		if mysqlDuplicate(err) {
			var existing string
			lookup := s.DB.QueryRowContext(ctx, `SELECT id FROM settlement_receipt WHERE tenant_id=? AND idempotency_key=?`, p.TenantID, key).Scan(&existing)
			return existing, lookup
		}
		return "", err
	}
	return id, nil
}

type AllocationInput struct {
	ReceiptID    string `json:"receipt_id"`
	ReceivableID string `json:"receivable_id"`
	Amount       string `json:"amount"`
	MatchMode    string `json:"match_mode"`
}

func (s *Service) ConfirmAllocation(ctx context.Context, p Principal, key string, in AllocationInput) (string, error) {
	a, err := amount(in.Amount)
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
	var receiptAvailable, receiptCurrency, receivableOpen, receivableCurrency, receiptCustomer, receivableCustomer string
	err = tx.QueryRowContext(ctx, `SELECT unallocated_amount,currency,customer_id FROM settlement_receipt WHERE id=? AND tenant_id=? AND status IN ('AVAILABLE','PARTIALLY_ALLOCATED') FOR UPDATE`, in.ReceiptID, p.TenantID).Scan(&receiptAvailable, &receiptCurrency, &receiptCustomer)
	if err == sql.ErrNoRows {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	err = tx.QueryRowContext(ctx, `SELECT r.open_amount,r.currency,cs.customer_id FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id WHERE r.id=? AND r.tenant_id=? AND r.recognition_status='CONFIRMED' AND r.collection_status IN ('UNPAID','PARTIALLY_SETTLED') FOR UPDATE`, in.ReceivableID, p.TenantID).Scan(&receivableOpen, &receivableCurrency, &receivableCustomer)
	if err == sql.ErrNoRows {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	if receiptCurrency != receivableCurrency {
		return "", fmt.Errorf("%w: currency mismatch", ErrInvalid)
	}
	if receiptCustomer != receivableCustomer {
		return "", fmt.Errorf("%w: receipt and receivable customers differ", ErrInvalid)
	}
	if decimalGreater(a, receiptAvailable) || decimalGreater(a, receivableOpen) {
		return "", ErrConflict
	}
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receipt_allocation(id,tenant_id,allocation_no,receipt_id,receivable_id,allocated_amount,currency,status,match_mode,confirmed_by,confirmed_at,idempotency_key,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),?,UTC_TIMESTAMP(3))`, id, p.TenantID, "AL-"+id[:12], in.ReceiptID, in.ReceivableID, a, receiptCurrency, "CONFIRMED", in.MatchMode, p.UserID, key)
	if err != nil {
		if mysqlDuplicate(err) {
			return "", ErrConflict
		}
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receipt SET unallocated_amount=unallocated_amount-?,status=CASE WHEN unallocated_amount-?=0 THEN 'FULLY_ALLOCATED' ELSE 'PARTIALLY_ALLOCATED' END,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, a, a, in.ReceiptID, p.TenantID)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET received_allocated_amount=received_allocated_amount+?,open_amount=open_amount-?,collection_status=CASE WHEN open_amount-?=0 THEN 'SETTLED' ELSE 'PARTIALLY_SETTLED' END,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, a, a, a, in.ReceivableID, p.TenantID)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func decimalGreater(left, right string) bool {
	a, _ := new(big.Rat).SetString(left)
	b, _ := new(big.Rat).SetString(right)
	return a.Cmp(b) > 0
}
func mysqlDuplicate(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "duplicate")
}
