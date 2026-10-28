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
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
	ErrInvalid    = errors.New("invalid input")
	ErrForbidden  = errors.New("forbidden")
	ErrDependency = errors.New("business dependency unavailable")
)

type CreditEventPublisher interface {
	Publish(ctx context.Context, tenant, eventID string, payload []byte) error
}

type Service struct {
	ProjectReferences   ProjectReferenceResolver
	RoleDirectory       RoleDirectory
	ERPProvider         ERPProvider
	DB                  *sql.DB
	CreditPublisher     CreditEventPublisher
	InvoiceIssuanceMode string
	TaxProviderCode     string
	// Optional only for directly constructed component tests. The HTTP API
	// installs its live commercial gate for every invoice archive operation.
	CheckInvoiceArchiveLicense func(context.Context) error
}

func (s *Service) taxProviderCode() string {
	if value := strings.TrimSpace(s.TaxProviderCode); value != "" {
		return value
	}
	return "default"
}

func (s *Service) invoiceIssuanceMode() string {
	if s.InvoiceIssuanceMode == "tax_adapter" {
		return "tax_adapter"
	}
	return "manual"
}

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
		OwnerUserID  string        `json:"owner_user_id"`
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
	TenantID, UserID      string
	Permissions           map[string]bool
	Roles                 []string
	DataScopes            []DataScope
	IdentityID            string
	PersonID              string
	DisplayName           string
	Username              string
	CatalogVersion        string
	AuthorizationRevision uint64
	// LoginIP 是会话创建时平台记录的登录地址（AUD-IP-2026）。它随会话固定，
	// 与每次操作观察到的客户端地址分开存储。
	LoginIP string
}

// DataScope 用途：保存平台返回的角色级可信应用范围。
type DataScope struct {
	RoleCode        string `json:"role_code"`
	ScopeType       string `json:"scope_type"`
	ScopeID         string `json:"scope_id"`
	EnvironmentCode string `json:"environment_code"`
}

// Allows 用途：校验可信有效权限，并兼容原管理能力包含的只读与反馈动作。
// 参数：permission 为接口要求的结算能力。
// 返回值：当前主体拥有该能力时返回真。
func (p Principal) Allows(permission string) bool {
	if p.Permissions[permission] || p.Permissions["settlement.admin"] {
		return true
	}
	switch permission {
	case "settlement.dunning.read", "settlement.dunning.feedback":
		return p.Permissions["settlement.dunning.manage"]
	case "settlement.receipt.read":
		return p.Permissions["settlement.receipt.record"]
	case "settlement.allocation.read":
		return p.Permissions["settlement.allocation.confirm"]
	}
	return false
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

var (
	moneyPattern      = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,2})?$`)
	sixDecimalPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,6})?$`)
)

func amount(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !moneyPattern.MatchString(value) {
		return "", fmt.Errorf("%w: amount must be a positive decimal with at most two fractional digits", ErrInvalid)
	}
	r, ok := new(big.Rat).SetString(value)
	if !ok || r.Sign() <= 0 {
		return "", fmt.Errorf("%w: amount must be a positive decimal with at most two fractional digits", ErrInvalid)
	}
	return r.FloatString(2), nil
}

// nonNegativeAmount 用于允许零金额的发票税额分量，
// 如零税率或免税行。其他金额入口仍
// 要求正金额。
func nonNegativeAmount(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !moneyPattern.MatchString(value) {
		return "", fmt.Errorf("%w: amount must be a non-negative decimal with at most two fractional digits", ErrInvalid)
	}
	r, ok := new(big.Rat).SetString(value)
	if !ok || r.Sign() < 0 {
		return "", fmt.Errorf("%w: amount must be a non-negative decimal with at most two fractional digits", ErrInvalid)
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

func textTooLong(value string, max int) bool {
	return len([]rune(strings.TrimSpace(value))) > max
}

// IngestContract 在创建财务快照前存储原始事件。
// 重放事件 ID 为成功空操作；若同一版本已存在但属于
// 其他事件 ID，则拒绝，防止旧合同更新覆盖事实。
// trustedTenant 是机器令牌验签得到的可信租户（AUD-2026-001）：报文声明的租户
// 必须与之一致，否则持合法令牌的调用方可声明任意租户写入财务快照与应收计划
// （对齐 ApplyTaxCallback 的租户一致性校验）。
func (s *Service) IngestContract(ctx context.Context, source, trustedTenant string, event ContractEvent) (bool, error) {
	if _, err := required(event.EventID, "event_id"); err != nil {
		return false, err
	}
	trustedTenant = strings.TrimSpace(trustedTenant)
	if trustedTenant == "" || event.TenantID != trustedTenant {
		return false, fmt.Errorf("%w: tenant_id must equal the verified machine tenant", ErrInvalid)
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
	// 首个财务生效事件固定合同财务
	// 基线。任何后续事件 ID，即使合同版本更新，均为
	// 修订，不得创建第二份全额应收。精确事件 ID
	// 重放在上方作为成功重复处理。
	if currentVersion != 0 {
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

type PlanConfirmationInput struct {
	Version       int    `json:"version"`
	DueDate       string `json:"due_date"`
	PlannedAmount string `json:"planned_amount"`
	Reason        string `json:"reason"`
}

// ConfirmPlan 用途：核对本人责任和合同预算后确认计划并生成应收。
// 参数：ctx为上下文，p为可信主体，id为计划ID，in为当前版本和调整字段。
// 返回值：成功无错误，授权、预算、版本或存储失败时返回错误。
func (s *Service) ConfirmPlan(ctx context.Context, p Principal, id string, in PlanConfirmationInput) error {
	if !p.Allows("settlement.receivable.confirm") {
		return ErrForbidden
	}
	if in.Version < 1 {
		return fmt.Errorf("%w: version must be positive", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var snapshot, due, planned, currency string
	err = tx.QueryRowContext(ctx, `SELECT rp.contract_snapshot_id,DATE_FORMAT(rp.due_date,'%Y-%m-%d'),rp.planned_amount,cs.currency FROM settlement_receivable_plan rp JOIN settlement_contract_snapshot cs ON cs.id=rp.contract_snapshot_id AND cs.tenant_id=rp.tenant_id WHERE rp.id=? AND rp.tenant_id=? AND rp.confirmation_status='PENDING_CONFIRMATION' AND rp.version=? FOR UPDATE`, id, p.TenantID, in.Version).Scan(&snapshot, &due, &planned, &currency)
	if err == sql.ErrNoRows {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if err = s.RequireContractAccessTx(ctx, tx, p, "settlement.receivable.confirm", snapshot); err != nil {
		return err
	}
	confirmedDue := strings.TrimSpace(in.DueDate)
	if confirmedDue == "" {
		confirmedDue = due
	}
	if _, parseErr := time.Parse("2006-01-02", confirmedDue); parseErr != nil {
		return fmt.Errorf("%w: due_date must be YYYY-MM-DD", ErrInvalid)
	}
	confirmedAmount := strings.TrimSpace(in.PlannedAmount)
	if confirmedAmount == "" {
		confirmedAmount = planned
	} else if confirmedAmount, err = amount(confirmedAmount); err != nil {
		return err
	}
	reason := strings.TrimSpace(in.Reason)
	if len(reason) > 500 {
		return fmt.Errorf("%w: reason is too long", ErrInvalid)
	}
	if (confirmedDue != due || confirmedAmount != planned) && reason == "" {
		return fmt.Errorf("%w: reason is required when plan values change", ErrInvalid)
	}
	if confirmedAmount != planned {
		if err = checkPlanBudget(ctx, tx, p.TenantID, snapshot, id, confirmedAmount); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable_plan SET due_date=?,planned_amount=?,confirmation_status='CONFIRMED',confirmation_reason=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND version=?`, confirmedDue, confirmedAmount, reason, id, p.TenantID, in.Version); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,receivable_plan_id,due_date,original_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newID(), p.TenantID, "AR-"+newID()[:12], snapshot, id, confirmedDue, confirmedAmount, confirmedAmount, currency, "CONFIRMED", "UNPAID", "NOT_INVOICED")
	if err != nil {
		return err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_RECEIVABLE_PLAN_CONFIRMED", "receivable_plan", id, map[string]any{
		"actor_id": p.UserID, "result": "SUCCESS", "risk_level": "HIGH",
		"old_due_date": due, "new_due_date": confirmedDue,
		"old_planned_amount": planned, "new_planned_amount": confirmedAmount,
		"reason": reason,
	}); err != nil {
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
	if in.CustomerID, err = required(in.CustomerID, "customer_id"); err != nil {
		return "", err
	}
	if in.CustomerName, err = required(in.CustomerName, "customer_name"); err != nil {
		return "", err
	}
	if in.BankReference, err = required(in.BankReference, "bank_transaction_reference"); err != nil {
		return "", err
	}
	if textTooLong(in.CustomerID, 128) || textTooLong(in.CustomerName, 255) || textTooLong(in.BankReference, 128) {
		return "", fmt.Errorf("%w: receipt text exceeds the length limit", ErrInvalid)
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if len(in.Currency) != 3 {
		return "", fmt.Errorf("%w: currency must be a three-letter code", ErrInvalid)
	}
	in.PaymentMethod = strings.ToUpper(strings.TrimSpace(in.PaymentMethod))
	if !map[string]bool{"TRANSFER": true, "BANK_ACCEPTANCE": true, "CHEQUE": true, "CASH": true}[in.PaymentMethod] {
		return "", fmt.Errorf("%w: invalid payment_method", ErrInvalid)
	}
	if _, err = time.Parse("2006-01-02", in.ReceiptDate); err != nil {
		return "", fmt.Errorf("%w: receipt_date must be YYYY-MM-DD", ErrInvalid)
	}
	id := newID()
	no := "RC-" + id[:12]
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receipt(id,tenant_id,receipt_no,customer_id,customer_name_snapshot,amount,unallocated_amount,currency,receipt_date,payment_method,bank_transaction_reference,source_type,status,idempotency_key,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, no, in.CustomerID, in.CustomerName, a, a, in.Currency, in.ReceiptDate, in.PaymentMethod, in.BankReference, "MANUAL", "AVAILABLE", key)
	if err != nil {
		if mysqlDuplicate(err) {
			var existing string
			lookup := tx.QueryRowContext(ctx, `SELECT id FROM settlement_receipt WHERE tenant_id=? AND idempotency_key=?`, p.TenantID, key).Scan(&existing)
			if lookup == nil {
				return existing, tx.Commit()
			}
			return "", ErrConflict
		}
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_RECEIPT_RECORDED", "receipt", id, map[string]any{"actor_id": p.UserID, "source_type": "MANUAL", "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

type AllocationInput struct {
	ReceiptID       string `json:"receipt_id"`
	ReceivableID    string `json:"receivable_id"`
	Amount          string `json:"amount"`
	MatchMode       string `json:"match_mode"`
	MatchConfidence int    `json:"match_confidence"`
}

func (s *Service) ConfirmAllocation(ctx context.Context, p Principal, key string, in AllocationInput) (string, error) {
	a, err := amount(in.Amount)
	if err != nil {
		return "", err
	}
	if _, err = required(key, "Idempotency-Key"); err != nil {
		return "", err
	}
	in.MatchMode = strings.ToUpper(strings.TrimSpace(in.MatchMode))
	if in.MatchMode != "MANUAL" && in.MatchMode != "SUGGESTED" {
		return "", fmt.Errorf("%w: invalid match_mode", ErrInvalid)
	}
	if in.MatchConfidence < 0 || in.MatchConfidence > 100 || (in.MatchMode == "SUGGESTED" && in.MatchConfidence == 0) {
		return "", fmt.Errorf("%w: match_confidence must be between 1 and 100 for suggested matches", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var receiptAvailable, receiptCurrency, receiptDate, receivableOpen, receivableOriginal, receivableCurrency, receiptCustomer, receivableCustomer string
	var dueDate, contractNo string
	var installment int
	err = tx.QueryRowContext(ctx, `SELECT unallocated_amount,currency,DATE_FORMAT(receipt_date,'%Y-%m-%d'),customer_id FROM settlement_receipt WHERE id=? AND tenant_id=? AND status IN ('AVAILABLE','PARTIALLY_ALLOCATED') FOR UPDATE`, in.ReceiptID, p.TenantID).Scan(&receiptAvailable, &receiptCurrency, &receiptDate, &receiptCustomer)
	if err == sql.ErrNoRows {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	err = tx.QueryRowContext(ctx, `SELECT r.open_amount,r.original_amount,r.currency,cs.customer_id,DATE_FORMAT(r.due_date,'%Y-%m-%d'),cs.source_contract_no,rp.installment_no FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id JOIN settlement_receivable_plan rp ON rp.id=r.receivable_plan_id AND rp.tenant_id=r.tenant_id WHERE r.id=? AND r.tenant_id=? AND r.recognition_status='CONFIRMED' AND r.collection_status IN ('UNPAID','PARTIALLY_SETTLED') FOR UPDATE`, in.ReceivableID, p.TenantID).Scan(&receivableOpen, &receivableOriginal, &receivableCurrency, &receivableCustomer, &dueDate, &contractNo, &installment)
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
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receipt_allocation(id,tenant_id,allocation_no,receipt_id,receivable_id,allocated_amount,currency,status,match_mode,match_confidence,confirmed_by,confirmed_at,idempotency_key,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),?,UTC_TIMESTAMP(3))`, id, p.TenantID, "AL-"+id[:12], in.ReceiptID, in.ReceivableID, a, receiptCurrency, "CONFIRMED", in.MatchMode, in.MatchConfidence, p.UserID, key)
	if err != nil {
		if mysqlDuplicate(err) {
			return "", ErrConflict
		}
		return "", err
	}
	receiptRemaining := new(big.Rat).Sub(add(receiptAvailable), add(a))
	receivableOpenAfter := new(big.Rat).Sub(add(receivableOpen), add(a))
	receiptStatus := "PARTIALLY_ALLOCATED"
	if receiptRemaining.Sign() == 0 {
		receiptStatus = "FULLY_ALLOCATED"
	}
	receivableStatus := "PARTIALLY_SETTLED"
	if receivableOpenAfter.Sign() == 0 {
		receivableStatus = "SETTLED"
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receipt SET unallocated_amount=?,status=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, receiptRemaining.FloatString(2), receiptStatus, in.ReceiptID, p.TenantID)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET open_amount=?,collection_status=?,received_allocated_amount=received_allocated_amount+?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, receivableOpenAfter.FloatString(2), receivableStatus, a, in.ReceivableID, p.TenantID)
	if err != nil {
		return "", err
	}
	var creditEventID string
	if receivableOpen == a {
		// CRM 信用使用数字客户 ID、字符串 period_no 及
		// RFC3339 日期。非 CRM 客户 ID 不创建信用同步
		// 事件，避免无效载荷污染重试队列。
		if customerID, parseErr := strconv.ParseUint(strings.TrimSpace(receivableCustomer), 10, 64); parseErr == nil {
			creditEventID = newID()
			payload, marshalErr := json.Marshal(map[string]any{
				"event_id": creditEventID, "event_type": creditEventCompleted,
				"tenant_id": p.TenantID, "payment_id": id, "receipt_id": in.ReceiptID,
				"allocation_id": id, "customer_id": customerID, "contract_no": contractNo,
				"period_no": strconv.Itoa(installment), "due_date": creditDate(dueDate), "paid_date": creditDate(receiptDate),
				"due_amount": receivableOriginal, "paid_amount": receivableOriginal, "source_system": "settlement",
			})
			if marshalErr != nil {
				return "", marshalErr
			}
			if err = insertCreditSync(ctx, tx, p.TenantID, creditEventID, id, in.ReceivableID, payload); err != nil {
				return "", err
			}
		}
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_ALLOCATION_CONFIRMED", "receipt_allocation", id, map[string]any{"actor_id": p.UserID, "receipt_id": in.ReceiptID, "receivable_id": in.ReceivableID, "match_mode": in.MatchMode, "match_confidence": in.MatchConfidence, "receivable_status": receivableStatus, "receipt_status": receiptStatus, "receivable_settled": receivableStatus == "SETTLED", "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if creditEventID != "" && s.CreditPublisher != nil {
		_ = s.RetryCreditSync(ctx, p.TenantID, creditEventID)
	}
	return id, nil
}

func creditDate(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 10 {
		return value + "T00:00:00Z"
	}
	return value
}

func decimalGreater(left, right string) bool {
	a, _ := new(big.Rat).SetString(left)
	b, _ := new(big.Rat).SetString(right)
	return a.Cmp(b) > 0
}
func mysqlDuplicate(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "duplicate")
}
