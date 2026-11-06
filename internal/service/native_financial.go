package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/j-s-te/settlement/internal/businessclock"
)

const NativeCustomerPrefix = "settlement_local:"
const NativeFinancialBasis = "LOCAL_FINANCIAL"

var (
	ErrNativeFinancialDisabled = errors.New("native financial baseline is disabled")
	ErrNativeFinancialConsumed = errors.New("native financial baseline has accounting facts")
	nativeCustomerPattern      = regexp.MustCompile(`^settlement_local:[A-F0-9]{32}$`)
	nativeMoneyPattern         = regexp.MustCompile(`^[0-9]{1,16}(\.[0-9]{1,2})?$`)
	nativeCurrencyPattern      = regexp.MustCompile(`^[A-Z]{3}$`)
)

type NativeCustomerInput struct {
	Name string `json:"name"`
}
type NativeFinancialCustomer struct {
	ID          string `json:"id"`
	CustomerKey string `json:"customer_key"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
}
type NativeFinancialInput struct {
	CustomerKey       string        `json:"customer_key"`
	DocumentNo        string        `json:"document_no"`
	Currency          string        `json:"currency"`
	Amount            string        `json:"amount"`
	EffectiveDate     string        `json:"effective_date"`
	EvidenceType      string        `json:"evidence_type"`
	EvidenceReference string        `json:"evidence_reference"`
	EvidenceNote      string        `json:"evidence_note"`
	Reason            string        `json:"reason"`
	PaymentTerms      []PaymentTerm `json:"payment_terms"`
}
type NativeFinancialBaseline struct {
	ID                string        `json:"id"`
	BasisType         string        `json:"basis_type"`
	LocalDocumentID   string        `json:"local_document_id"`
	LocalDocumentNo   string        `json:"local_document_no"`
	Version           int           `json:"version"`
	CustomerKey       string        `json:"customer_key"`
	CustomerName      string        `json:"customer_name"`
	CustomerNamespace string        `json:"customer_namespace"`
	Currency          string        `json:"currency"`
	Amount            string        `json:"amount"`
	EffectiveDate     string        `json:"effective_date"`
	Status            string        `json:"status"`
	EvidenceType      string        `json:"evidence_type"`
	EvidenceReference string        `json:"evidence_reference"`
	EvidenceNote      string        `json:"evidence_note"`
	SourceContractID  *string       `json:"source_contract_id"`
	SourceContractNo  string        `json:"source_contract_no"`
	PaymentTerms      []PaymentTerm `json:"payment_terms,omitempty"`
}
type NativeFinancialVoidInput struct {
	Version int    `json:"version"`
	Reason  string `json:"reason"`
}

// ConfigureFinancialSources 区分正式配置的订阅与既有零值服务构造。
// 参数：nativeEnabled 为本地基线能力，creditSubscribed 为已配置的 CRM 信用订阅。
// 返回值：无；不由客户端或发布者可达性推断订阅。
func (s *Service) ConfigureFinancialSources(nativeEnabled, creditSubscribed bool) {
	s.NativeFinancialEnabled = nativeEnabled
	s.CreditSyncSubscribed = new(bool)
	*s.CreditSyncSubscribed = creditSubscribed
}

// CanCreateNativeFinancial 仅允许已有全范围财务经理或管理员登记本地基线。
// 参数：p 为真实会话主体。
// 返回值：能力、原权限与范围同时满足时返回真。
func (s *Service) CanCreateNativeFinancial(p Principal) bool {
	if !s.NativeFinancialEnabled || p.TenantID == "" || p.UserID == "" ||
		!p.Allows("settlement.receivable.create") || !p.Unrestricted("settlement.receivable.create") {
		return false
	}
	for _, role := range p.Roles {
		if role == "settlement_admin" || role == "settlement_finance_manager" {
			return true
		}
	}
	return false
}

// requireNativeFinancial 统一校验本地能力、权限与主体范围。
// 参数：p 为真实主体，write 区分创建或读取。
// 返回值：未启用或范围不足时返回错误。
func (s *Service) requireNativeFinancial(p Principal, write bool) error {
	if !s.NativeFinancialEnabled {
		return ErrNativeFinancialDisabled
	}
	if write {
		if !s.CanCreateNativeFinancial(p) {
			return ErrForbidden
		}
		return nil
	}
	if p.TenantID == "" || p.UserID == "" || !p.Unrestricted("settlement.receivable.read") {
		return ErrForbidden
	}
	return nil
}

// nativeIdempotencyKey 校验财务写命令的显式幂等坐标。
// 参数：key 为请求头中的键。
// 返回值：规范键或输入错误。
func nativeIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 128 {
		return "", ErrInvalid
	}
	return key, nil
}

// CreateNativeFinancialCustomer 创建不依赖 CRM 的本租户客户注册记录。
// 参数：ctx/p 为授权上下文，key 为幂等键，in 为客户名称。
// 返回值：不可与 CRM 数字 ID 混淆的服务端客户引用。
func (s *Service) CreateNativeFinancialCustomer(ctx context.Context, p Principal, key string, in NativeCustomerInput) (NativeFinancialCustomer, error) {
	if err := s.requireNativeFinancial(p, true); err != nil {
		return NativeFinancialCustomer{}, err
	}
	key, err := nativeIdempotencyKey(key)
	if err != nil {
		return NativeFinancialCustomer{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || textTooLong(in.Name, 255) || strings.ContainsRune(in.Name, '\x00') {
		return NativeFinancialCustomer{}, ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return NativeFinancialCustomer{}, err
	}
	defer tx.Rollback()
	id := newID()
	id, replay, err := claimIdempotency(ctx, tx, p.TenantID, "CREATE_FINANCIAL_CUSTOMER", key, id, struct {
		Actor string
		Input NativeCustomerInput
	}{p.UserID, in})
	if err != nil {
		return NativeFinancialCustomer{}, err
	}
	result := NativeFinancialCustomer{ID: id, CustomerKey: NativeCustomerPrefix + id, Namespace: "settlement_local", Name: in.Name}
	if replay {
		return result, tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_financial_customer(id,tenant_id,name,status,created_by,created_at) VALUES(?,?,?,'ACTIVE',?,UTC_TIMESTAMP(3))`, id, p.TenantID, in.Name, p.UserID); err != nil {
		return NativeFinancialCustomer{}, err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_FINANCIAL_CUSTOMER_CREATED", "financial_customer", id, map[string]any{"actor_id": p.UserID, "customer_key": result.CustomerKey, "result": "SUCCESS"}); err != nil {
		return NativeFinancialCustomer{}, err
	}
	return result, tx.Commit()
}

// normalizeNativeFinancial 校验本地财务依据与精确付款计划，不生成外部身份。
// 参数：in 为客户及金额依据，now 为服务端当前时刻。
// 返回值：规范请求；非法金额、日期、依据或计划返回错误。
func normalizeNativeFinancial(in NativeFinancialInput, now time.Time) (NativeFinancialInput, error) {
	in.CustomerKey = strings.TrimSpace(in.CustomerKey)
	in.DocumentNo = strings.TrimSpace(in.DocumentNo)
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.Amount = strings.TrimSpace(in.Amount)
	in.EvidenceType = strings.TrimSpace(in.EvidenceType)
	in.EvidenceReference = strings.TrimSpace(in.EvidenceReference)
	in.EvidenceNote = strings.TrimSpace(in.EvidenceNote)
	in.Reason = strings.TrimSpace(in.Reason)
	validEvidence := in.EvidenceType == "OFFLINE_DOCUMENT" || in.EvidenceType == "PURCHASE_ORDER" || in.EvidenceType == "CUSTOMER_CONFIRMATION" || in.EvidenceType == "OTHER"
	if !nativeCustomerPattern.MatchString(in.CustomerKey) || !nativeCurrencyPattern.MatchString(in.Currency) ||
		!validEvidence || in.EvidenceReference == "" || in.EvidenceNote == "" || in.Reason == "" ||
		textTooLong(in.DocumentNo, 128) || textTooLong(in.EvidenceReference, 200) || textTooLong(in.EvidenceNote, 1000) || textTooLong(in.Reason, 500) ||
		strings.ContainsRune(in.DocumentNo+in.EvidenceReference+in.EvidenceNote+in.Reason, '\x00') || !nativeMoneyPattern.MatchString(in.Amount) {
		return in, ErrInvalid
	}
	var err error
	in.Amount, err = amount(in.Amount)
	if err != nil {
		return in, err
	}
	date, err := time.Parse("2006-01-02", in.EffectiveDate)
	if err != nil || date.Year() < 1000 || in.EffectiveDate > now.In(businessclock.Location()).Format("2006-01-02") || len(in.PaymentTerms) < 1 || len(in.PaymentTerms) > 100 {
		return in, ErrInvalid
	}
	in.PaymentTerms = append([]PaymentTerm(nil), in.PaymentTerms...)
	sort.Slice(in.PaymentTerms, func(i, j int) bool { return in.PaymentTerms[i].InstallmentNo < in.PaymentTerms[j].InstallmentNo })
	total := new(big.Rat)
	seen := map[int]bool{}
	for i := range in.PaymentTerms {
		term := &in.PaymentTerms[i]
		term.Amount = strings.TrimSpace(term.Amount)
		day, dateErr := time.Parse("2006-01-02", term.DueDate)
		if term.InstallmentNo < 1 || seen[term.InstallmentNo] || dateErr != nil || day.Year() < 1000 || !nativeMoneyPattern.MatchString(term.Amount) {
			return in, ErrInvalid
		}
		seen[term.InstallmentNo] = true
		term.Amount, err = amount(term.Amount)
		if err != nil {
			return in, err
		}
		total.Add(total, add(term.Amount))
	}
	if total.Cmp(add(in.Amount)) != 0 {
		return in, fmt.Errorf("%w: 付款计划合计必须等于财务依据金额", ErrInvalid)
	}
	return in, nil
}

// CreateNativeFinancialBaseline 原子登记自有财务依据与待确认计划。
// 参数：ctx/p 为授权上下文，key 为幂等键，in 为完整依据及分期。
// 返回值：内部财务基线；不会制造合同事件、合同身份或责任记录。
func (s *Service) CreateNativeFinancialBaseline(ctx context.Context, p Principal, key string, in NativeFinancialInput) (NativeFinancialBaseline, error) {
	if err := s.requireNativeFinancial(p, true); err != nil {
		return NativeFinancialBaseline{}, err
	}
	key, err := nativeIdempotencyKey(key)
	if err != nil {
		return NativeFinancialBaseline{}, err
	}
	in, err = normalizeNativeFinancial(in, time.Now().UTC())
	if err != nil {
		return NativeFinancialBaseline{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return NativeFinancialBaseline{}, err
	}
	defer tx.Rollback()
	var customerName string
	if err = tx.QueryRowContext(ctx, `SELECT name FROM settlement_financial_customer WHERE tenant_id=? AND id=? AND status='ACTIVE' FOR SHARE`, p.TenantID, strings.TrimPrefix(in.CustomerKey, NativeCustomerPrefix)).Scan(&customerName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return NativeFinancialBaseline{}, err
	}
	id := newID()
	id, replay, err := claimIdempotency(ctx, tx, p.TenantID, "CREATE_NATIVE_FINANCIAL", key, id, struct {
		Actor string
		Input NativeFinancialInput
	}{p.UserID, in})
	if err != nil {
		return NativeFinancialBaseline{}, err
	}
	if replay {
		if err = tx.Commit(); err != nil {
			return NativeFinancialBaseline{}, err
		}
		return s.getNativeFinancialBaseline(ctx, p.TenantID, id)
	}
	if in.DocumentNo == "" {
		in.DocumentNo = "FB-" + id[:12]
	}
	effective, _ := time.ParseInLocation("2006-01-02", in.EffectiveDate, businessclock.Location())
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,basis_type,local_customer_id,local_document_id,local_document_no,local_document_version,customer_namespace,local_evidence_type,local_evidence_reference,local_evidence_note,local_created_by,created_at,updated_at) VALUES(?,?,'settlement_local',?,NULL,'',0,?,?,?,?,'EFFECTIVE',?,'LOCAL_FINANCIAL',?,?,?,1,'settlement_local',?,?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, id, in.CustomerKey, customerName, in.Currency, in.Amount, effective.UTC(), strings.TrimPrefix(in.CustomerKey, NativeCustomerPrefix), id, in.DocumentNo, in.EvidenceType, in.EvidenceReference, in.EvidenceNote, p.UserID)
	if err != nil {
		if mysqlDuplicate(err) {
			err = ErrConflict
		}
		return NativeFinancialBaseline{}, err
	}
	for _, term := range in.PaymentTerms {
		if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,created_at,updated_at) VALUES(?,?,?,?,?,?,'LOCAL_FINANCIAL','PENDING_CONFIRMATION',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newID(), p.TenantID, id, term.InstallmentNo, term.DueDate, term.Amount); err != nil {
			return NativeFinancialBaseline{}, err
		}
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_NATIVE_FINANCIAL_CREATED", "financial_baseline", id, map[string]any{"actor_id": p.UserID, "basis_type": NativeFinancialBasis, "local_document_no": in.DocumentNo, "customer_key": in.CustomerKey, "amount": in.Amount, "reason": in.Reason, "evidence_type": in.EvidenceType, "evidence_reference": in.EvidenceReference, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return NativeFinancialBaseline{}, err
	}
	result := NativeFinancialBaseline{ID: id, BasisType: NativeFinancialBasis, LocalDocumentID: id, LocalDocumentNo: in.DocumentNo, Version: 1, CustomerKey: in.CustomerKey, CustomerName: customerName, CustomerNamespace: "settlement_local", Currency: in.Currency, Amount: in.Amount, EffectiveDate: in.EffectiveDate, Status: "EFFECTIVE", EvidenceType: in.EvidenceType, EvidenceReference: in.EvidenceReference, EvidenceNote: in.EvidenceNote, PaymentTerms: in.PaymentTerms}
	return result, tx.Commit()
}
