package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const createInvoiceRequestCommand = "CREATE_INVOICE_REQUEST"

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
	ContractSnapshotID string                   `json:"contract_snapshot_id"`
	BuyerProfile       map[string]any           `json:"buyer_profile"`
	InvoiceType        string                   `json:"invoice_type"`
	Items              []InvoiceItemInput       `json:"items"`
	Allocations        []InvoiceAllocationInput `json:"allocations"`
}

func decimal(value string, positive bool) (string, error) {
	value = strings.TrimSpace(value)
	if !sixDecimalPattern.MatchString(value) {
		return "", fmt.Errorf("%w: invalid decimal", ErrInvalid)
	}
	r, ok := new(big.Rat).SetString(value)
	if !ok || r.Sign() < 0 || (positive && r.Sign() == 0) {
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

// incrementInvoiced 锁定应收，并写入明确的新已开票
// 余额。不能使用 UPDATE 正在修改的同一列计算状态，
// 因为 MySQL 以已更新行求值后续赋值。
func incrementInvoiced(ctx context.Context, tx *sql.Tx, tenant, receivableID, value string) error {
	var current, original string
	if err := tx.QueryRowContext(ctx, `SELECT invoiced_amount,original_amount FROM settlement_receivable WHERE id=? AND tenant_id=? FOR UPDATE`, receivableID, tenant).Scan(&current, &original); err != nil {
		return err
	}
	next := new(big.Rat).Add(add(current), add(value))
	status := "PARTIALLY_INVOICED"
	if next.Cmp(add(original)) >= 0 {
		status = "FULLY_INVOICED"
	}
	_, err := tx.ExecContext(ctx, `UPDATE settlement_receivable SET invoiced_amount=?,invoice_status=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, next.FloatString(2), status, receivableID, tenant)
	return err
}

// decrementInvoiced 对红冲或冲销执行与 incrementInvoiced 对应的反向操作。
// 既有行为将下限限制为零，状态依据限制后的值。
func decrementInvoiced(ctx context.Context, tx *sql.Tx, tenant, receivableID, value string) error {
	var current, original string
	if err := tx.QueryRowContext(ctx, `SELECT invoiced_amount,original_amount FROM settlement_receivable WHERE id=? AND tenant_id=? FOR UPDATE`, receivableID, tenant).Scan(&current, &original); err != nil {
		return err
	}
	next := new(big.Rat).Sub(add(current), add(value))
	if next.Sign() < 0 {
		next = new(big.Rat)
	}
	status := "PARTIALLY_INVOICED"
	if next.Sign() == 0 {
		status = "NOT_INVOICED"
	} else if next.Cmp(add(original)) >= 0 {
		status = "FULLY_INVOICED"
	}
	_, err := tx.ExecContext(ctx, `UPDATE settlement_receivable SET invoiced_amount=?,invoice_status=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, next.FloatString(2), status, receivableID, tenant)
	return err
}

// RequestMeta 保存本次请求可写入审计的来源信息（AUD-IP-2026）。
type RequestMeta struct {
	LoginIP, ClientIP, IPSource string
}

type requestMetaKey struct{}

// WithRequestMeta 由 HTTP 鉴权层在请求进入业务前写入。
func WithRequestMeta(ctx context.Context, meta RequestMeta) context.Context {
	return context.WithValue(ctx, requestMetaKey{}, meta)
}

// RequestMetaFrom 读取请求来源信息；没有时返回零值。
func RequestMetaFrom(ctx context.Context) RequestMeta {
	meta, _ := ctx.Value(requestMetaKey{}).(RequestMeta)
	return meta
}

// enrichAuditIP 把来源地址并入审计 detail；业务已显式提供时不覆盖。
func enrichAuditIP(payload map[string]any, ctx context.Context) {
	meta := RequestMetaFrom(ctx)
	if _, ok := payload["login_ip"]; !ok && meta.LoginIP != "" {
		payload["login_ip"] = meta.LoginIP
	}
	if _, ok := payload["client_ip"]; !ok && meta.ClientIP != "" {
		payload["client_ip"] = meta.ClientIP
	}
	if _, ok := payload["ip_source"]; !ok && meta.IPSource != "" {
		payload["ip_source"] = meta.IPSource
	}
}

// enqueueCreditReversal 撤销应收当前结清轮次的
// 结清事实。完成事件按应收及事件类型定位，
// 而非按被冲销分配定位：多个分配共同
// 结清一期时，完成事实属于关闭该期的分配，
// 冲销则可能针对较早的分配。
//
// 尚未投递的完成事件被取消。已投递或进行中的
// 完成事实由冲销事件补偿，该事件引用
// 原事件及真正关闭该期的分配。
// 已有冲销事件的完成事实绝不再次补偿，
// 确保重复或重叠冲销仍保持幂等。
func enqueueCreditReversal(ctx context.Context, tx *sql.Tx, tenant, allocationID, receivableID string) (string, error) {
	var originalEventID, eventType, status, settledAllocationID string
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT completed.event_id,completed.event_type,completed.status,completed.allocation_id,completed.payload_json
		FROM settlement_credit_sync completed
		WHERE completed.tenant_id=? AND completed.receivable_id=? AND completed.event_type=? AND completed.status<>'CANCELLED'
		  AND NOT EXISTS (
		    SELECT 1 FROM settlement_credit_sync compensating
		    WHERE compensating.tenant_id=completed.tenant_id AND compensating.event_type=? AND compensating.original_event_id=completed.event_id
		  )
		ORDER BY completed.created_at DESC,completed.id DESC LIMIT 1 FOR UPDATE`, tenant, receivableID, creditEventCompleted, creditEventReversed).Scan(&originalEventID, &eventType, &status, &settledAllocationID, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if eventType == creditEventReversed || status == "CANCELLED" {
		return "", nil
	}
	if status == "PENDING" || status == "RETRY_WAIT" {
		_, err = tx.ExecContext(ctx, `UPDATE settlement_credit_sync SET status='CANCELLED',updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND event_id=? AND status IN ('PENDING','RETRY_WAIT')`, tenant, originalEventID)
		return "", err
	}
	var original struct {
		CustomerID uint64 `json:"customer_id"`
		ContractNo string `json:"contract_no"`
		PeriodNo   string `json:"period_no"`
		DueDate    string `json:"due_date"`
		PaidDate   string `json:"paid_date"`
		DueAmount  string `json:"due_amount"`
	}
	if err = json.Unmarshal(payload, &original); err != nil {
		return "", err
	}
	reversalEventID := newID()
	reversalPayload, err := json.Marshal(map[string]any{
		"event_id": reversalEventID, "event_type": creditEventReversed, "tenant_id": tenant,
		"payment_id": reversalEventID, "reversal_of_payment_id": settledAllocationID,
		"customer_id": original.CustomerID, "contract_no": original.ContractNo,
		"period_no": original.PeriodNo, "due_date": original.DueDate, "paid_date": original.PaidDate,
		"due_amount": original.DueAmount, "paid_amount": "0.00", "source_system": "settlement",
	})
	if err != nil {
		return "", err
	}
	if err = insertCreditSyncEvent(ctx, tx, tenant, reversalEventID, allocationID, receivableID, creditEventReversed, originalEventID, reversalPayload); err != nil {
		return "", err
	}
	return reversalEventID, nil
}

func outbox(ctx context.Context, tx *sql.Tx, tenant, destination, eventType, aggregateType, aggregateID string, payload any) error {
	id := newID()
	if destination == "TAX_INVOICE_COMMAND" {
		if source, ok := payload.(map[string]any); ok {
			envelope := make(map[string]any, len(source)+5)
			for key, value := range source {
				envelope[key] = value
			}
			envelope["event_id"] = id
			envelope["event_type"] = eventType
			envelope["schema_version"] = 1
			envelope["tenant_id"] = tenant
			envelope["occurred_at"] = time.Now().UTC()
			payload = envelope
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_outbox_event(id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,status,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?, 'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, id, destination, eventType, aggregateType, aggregateID, body)
	if err != nil {
		return err
	}
	if destination == "PLATFORM_AUDIT" {
		// AUD-IP-2026：审计 detail 必须携带三类来源地址，平台据此区分
		// 登录地址、操作地址与投递地址，而不是拿投递连接地址冒充用户地址。
		if value, ok := payload.(map[string]any); ok {
			enrichAuditIP(value, ctx)
		}
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

// normalizeInvoiceItems 是发票明细运算的服务端唯一权威
// 来源。数量、单价和税率最多接受六位小数；
// 金额分量四舍五入到两位，必须与提交值
// 精确相等。任何尾差均拒绝，不自动吸收。
func normalizeInvoiceItems(items []InvoiceItemInput) ([]InvoiceItemInput, *big.Rat, *big.Rat, *big.Rat, error) {
	excl, tax, incl := new(big.Rat), new(big.Rat), new(big.Rat)
	normalized := make([]InvoiceItemInput, 0, len(items))
	for _, item := range items {
		q, err := decimal(item.Quantity, true)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		price, err := decimal(item.UnitPriceExclTax, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ae, err := amount(item.AmountExclTax)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		tr, err := decimal(item.TaxRate, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ta, err := nonNegativeAmount(item.TaxAmount)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ai, err := amount(item.AmountInclTax)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		quantity, _ := new(big.Rat).SetString(q)
		unitPrice, _ := new(big.Rat).SetString(price)
		expectedExcl := new(big.Rat).Mul(quantity, unitPrice).FloatString(2)
		taxRate, _ := new(big.Rat).SetString(tr)
		expectedExclRat, _ := new(big.Rat).SetString(expectedExcl)
		expectedTax := new(big.Rat).Mul(expectedExclRat, taxRate).FloatString(2)
		expectedIncl := add(expectedExcl, expectedTax).FloatString(2)
		if ae != expectedExcl || ta != expectedTax || ai != expectedIncl {
			return nil, nil, nil, nil, fmt.Errorf("%w: invoice line does not match quantity, unit price, tax rate and totals", ErrInvalid)
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
	return normalized, excl, tax, incl, nil
}

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
	// 与较早明确失败矛盾的迟到成功必须
	// 对账，不能随后重新尝试开具。
	var providerConflicts int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_tax_result_inbox ti JOIN settlement_invoice_issue_attempt ia ON ia.id=ti.aggregate_id AND ia.tenant_id=ti.tenant_id WHERE ia.invoice_request_id=? AND ia.tenant_id=? AND ti.operation_type='INVOICE_ISSUE' AND ti.processing_status='PROVIDER_CONFLICT'`, id, p.TenantID).Scan(&providerConflicts); err != nil {
		return "", err
	}
	if providerConflicts != 0 {
		return "", fmt.Errorf("%w: 税控结果存在冲突，禁止重新开票", ErrConflict)
	}
	attempt := newID()
	channel := "MANUAL"
	if s.invoiceIssuanceMode() == "tax_adapter" {
		channel = "TAX_ADAPTER"
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='ISSUE_PENDING',approved_by=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND version=?`, p.UserID, id, p.TenantID, version)
	if err != nil {
		return "", err
	}
	providerCode := ""
	if channel == "TAX_ADAPTER" {
		providerCode = s.taxProviderCode()
	}
	var attemptNo int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt_no),0)+1 FROM settlement_invoice_issue_attempt WHERE invoice_request_id=? AND tenant_id=?`, id, p.TenantID).Scan(&attemptNo); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_issue_attempt(id,tenant_id,invoice_request_id,attempt_no,channel,provider_code,external_request_id,idempotency_key,status,requested_at) VALUES(?,?,?,?,?,?,?,?,'PENDING',UTC_TIMESTAMP(3))`, attempt, p.TenantID, id, attemptNo, channel, providerCode, attempt, attempt)
	if err != nil {
		return "", err
	}
	if channel == "TAX_ADAPTER" {
		command, commandErr := invoiceIssueCommand(ctx, tx, p.TenantID, id, attempt, s.taxProviderCode())
		if commandErr != nil {
			return "", commandErr
		}
		if err = outbox(ctx, tx, p.TenantID, "TAX_INVOICE_COMMAND", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "invoice_request", id, command); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET command_event_id=(SELECT event_id FROM settlement_outbox_event WHERE tenant_id=? AND destination='TAX_INVOICE_COMMAND' AND event_type='SETTLEMENT_INVOICE_ISSUE_REQUESTED' AND aggregate_id=? ORDER BY created_at DESC,id DESC LIMIT 1) WHERE id=? AND tenant_id=?`, p.TenantID, id, attempt, p.TenantID); err != nil {
			return "", err
		}
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_APPROVED", "invoice_request", id, map[string]any{"actor_id": p.UserID, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return attempt, tx.Commit()
}

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
	return map[string]any{"operation_type": "INVOICE_ISSUE", "provider_code": provider, "external_request_id": attemptID, "attempt_id": attemptID, "invoice_request_id": requestID, "request_no": requestNo, "invoice_type": invoiceType, "currency": currency, "buyer_profile": buyerProfile, "amount_excl_tax": excl, "tax_amount": tax, "amount_incl_tax": incl, "items": items}, nil
}

// RejectInvoiceRequest 在同一事务内持久化审核决定，
// 并释放预留应收金额。审核人不能处理
// 自己的申请。
func (s *Service) RejectInvoiceRequest(ctx context.Context, p Principal, id string, version int, reason string) error {
	reason, err := required(reason, "reason")
	if err != nil {
		return err
	}
	if len(reason) > 500 {
		return fmt.Errorf("%w: reason is too long", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var applicant string
	err = tx.QueryRowContext(ctx, `SELECT submitted_by FROM settlement_invoice_request WHERE id=? AND tenant_id=? AND status='SUBMITTED' AND version=? FOR UPDATE`, id, p.TenantID, version).Scan(&applicant)
	if err != nil {
		return ErrConflict
	}
	if applicant == p.UserID {
		return fmt.Errorf("%w: applicant cannot review own invoice", ErrInvalid)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='REJECTED',approved_by=?,rejection_reason=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND version=?`, p.UserID, reason, id, p.TenantID, version); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request_allocation SET released_amount=reserved_amount,status='RELEASED' WHERE tenant_id=? AND invoice_request_id=? AND status='RESERVED'`, p.TenantID, id); err != nil {
		return err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_REJECTED", "invoice_request", id, map[string]any{"actor_id": p.UserID, "reason": reason, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return err
	}
	return tx.Commit()
}

type ManualInvoiceInput struct {
	InvoiceCode   string         `json:"invoice_code"`
	InvoiceNo     string         `json:"invoice_no"`
	IssueDate     string         `json:"issue_date"`
	SellerProfile map[string]any `json:"seller_profile"`
}

// RegisterManualInvoice 是安全的第一阶段适配器：可关闭
// 已审批请求，无需将核心台账耦合到特定税控厂商。
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
	var kind, currency, excl, tax, incl string
	var buyer []byte
	if err = tx.QueryRowContext(ctx, `SELECT ir.invoice_type,cs.currency,ir.amount_excl_tax,ir.tax_amount,ir.amount_incl_tax,ir.buyer_profile_snapshot FROM settlement_invoice_request ir JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id JOIN settlement_invoice_issue_attempt ia ON ia.invoice_request_id=ir.id AND ia.tenant_id=ir.tenant_id AND ia.channel='MANUAL' AND ia.status='PENDING' WHERE ir.id=? AND ir.tenant_id=? AND ir.status='ISSUE_PENDING' FOR UPDATE`, requestID, p.TenantID).Scan(&kind, &currency, &excl, &tax, &incl, &buyer); err != nil {
		return "", ErrConflict
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if len(currency) != 3 {
		return "", fmt.Errorf("%w: invoice currency is missing", ErrInvalid)
	}
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_tax_invoice(id,tenant_id,invoice_request_id,invoice_code,invoice_no,invoice_type,currency,amount_excl_tax,tax_amount,amount_incl_tax,issue_date,buyer_profile_snapshot,seller_profile_snapshot,status,issued_by_channel,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'ISSUED','MANUAL',UTC_TIMESTAMP(3))`, id, p.TenantID, requestID, code, no, kind, currency, excl, tax, incl, in.IssueDate, buyer, seller)
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

func (s *Service) ArchiveInvoiceDocument(ctx context.Context, p Principal, invoiceID, documentType, originalName, mediaType, objectID, checksum string) (string, error) {
	if _, err := required(objectID, "storage_object_id"); err != nil {
		return "", err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := archiveInvoiceDocumentTx(ctx, tx, p, invoiceID, documentType, originalName, mediaType, objectID, checksum)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

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
	var existing, existingInvoiceID string
	err = tx.QueryRowContext(ctx, `SELECT id,original_invoice_id FROM settlement_invoice_red_flush_request WHERE tenant_id=? AND idempotency_key=?`, p.TenantID, key).Scan(&existing, &existingInvoiceID)
	if err == nil {
		if existingInvoiceID != invoiceID {
			return "", fmt.Errorf("%w: idempotency key belongs to another invoice", ErrConflict)
		}
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var invoiceStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM settlement_tax_invoice WHERE id=? AND tenant_id=? FOR UPDATE`, invoiceID, p.TenantID).Scan(&invoiceStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if invoiceStatus != "ISSUED" {
		return "", fmt.Errorf("%w: invoice is not eligible for red flush", ErrConflict)
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_invoice_red_flush_request WHERE tenant_id=? AND original_invoice_id=? AND status IN ('SUBMITTED','ISSUE_PENDING','PROCESSING')`, p.TenantID, invoiceID).Scan(&active); err != nil {
		return "", err
	}
	if active > 0 {
		return "", fmt.Errorf("%w: active red flush request already exists", ErrConflict)
	}
	id := newID()
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_red_flush_request(id,tenant_id,original_invoice_id,reason_code,reason_detail,idempotency_key,status,requested_by,created_at,updated_at) VALUES(?,?,?,?,?,?,'SUBMITTED',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, invoiceID, reasonCode, reason, key, p.UserID); err != nil {
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

func (s *Service) ReviewInvoiceRedFlush(ctx context.Context, p Principal, id string, approve bool, in InvoiceRedFlushReviewInput) error {
	if in.Version < 1 {
		return fmt.Errorf("%w: version is required", ErrInvalid)
	}
	reason := strings.TrimSpace(in.Reason)
	if !approve && reason == "" {
		return fmt.Errorf("%w: rejection reason is required", ErrInvalid)
	}
	if len(reason) > 500 {
		return fmt.Errorf("%w: reason is too long", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var invoiceID, reasonCode, reasonDetail, requestedBy, reviewedBy, reviewReason, status string
	var version int
	err = tx.QueryRowContext(ctx, `SELECT original_invoice_id,reason_code,reason_detail,requested_by,reviewed_by,review_reason,status,version FROM settlement_invoice_red_flush_request WHERE id=? AND tenant_id=? FOR UPDATE`, id, p.TenantID).Scan(&invoiceID, &reasonCode, &reasonDetail, &requestedBy, &reviewedBy, &reviewReason, &status, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if approve && status == "ISSUE_PENDING" && reviewedBy == p.UserID {
		return tx.Commit()
	}
	if !approve && status == "REJECTED" && reviewedBy == p.UserID && reviewReason == reason {
		return tx.Commit()
	}
	if status != "SUBMITTED" || version != in.Version {
		return ErrConflict
	}
	if requestedBy == p.UserID {
		return fmt.Errorf("%w: requester cannot review own red flush request", ErrConflict)
	}
	if approve {
		if s.invoiceIssuanceMode() != "tax_adapter" {
			return fmt.Errorf("%w: red flush approval requires tax_adapter issuance mode", ErrConflict)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_request SET status='ISSUE_PENDING',reviewed_by=?,review_reason='',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status='SUBMITTED' AND version=?`, p.UserID, id, p.TenantID, in.Version); err != nil {
			return err
		}
		attemptID := newID()
		if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_red_flush_attempt(id,tenant_id,red_flush_request_id,original_invoice_id,attempt_no,provider_code,external_request_id,status,requested_at) VALUES(?,?,?,?,1,?,?,'PENDING',UTC_TIMESTAMP(3))`, attemptID, p.TenantID, id, invoiceID, s.taxProviderCode(), attemptID); err != nil {
			return err
		}
		var invoiceCode, invoiceNo, invoiceType, currency, excl, tax, incl, providerCode string
		var externalInvoiceID sql.NullString
		if err = tx.QueryRowContext(ctx, `SELECT invoice_code,invoice_no,invoice_type,currency,amount_excl_tax,tax_amount,amount_incl_tax,provider_code,external_invoice_id FROM settlement_tax_invoice WHERE id=? AND tenant_id=?`, invoiceID, p.TenantID).Scan(&invoiceCode, &invoiceNo, &invoiceType, &currency, &excl, &tax, &incl, &providerCode, &externalInvoiceID); err != nil {
			return err
		}
		command := map[string]any{"operation_type": "RED_FLUSH", "provider_code": s.taxProviderCode(), "external_request_id": attemptID, "red_flush_attempt_id": attemptID, "red_flush_request_id": id, "original_invoice_id": invoiceID, "original_provider_code": providerCode, "original_external_invoice_id": externalInvoiceID.String, "invoice_code": invoiceCode, "invoice_no": invoiceNo, "invoice_type": invoiceType, "currency": currency, "amount_excl_tax": excl, "tax_amount": tax, "amount_incl_tax": incl, "reason_code": reasonCode, "reason_detail": reasonDetail}
		if err = outbox(ctx, tx, p.TenantID, "TAX_INVOICE_COMMAND", "SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED", "tax_invoice", invoiceID, command); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_attempt SET command_event_id=(SELECT event_id FROM settlement_outbox_event WHERE tenant_id=? AND destination='TAX_INVOICE_COMMAND' AND event_type='SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED' AND aggregate_id=? ORDER BY created_at DESC,id DESC LIMIT 1) WHERE id=? AND tenant_id=?`, p.TenantID, invoiceID, attemptID, p.TenantID); err != nil {
			return err
		}
		if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_RED_FLUSH_APPROVED", "invoice_red_flush_request", id, map[string]any{"actor_id": p.UserID, "invoice_id": invoiceID, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
			return err
		}
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_request SET status='REJECTED',reviewed_by=?,review_reason=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status='SUBMITTED' AND version=?`, p.UserID, reason, id, p.TenantID, in.Version); err != nil {
			return err
		}
		if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_RED_FLUSH_REJECTED", "invoice_red_flush_request", id, map[string]any{"actor_id": p.UserID, "invoice_id": invoiceID, "reason": reason, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) AuditAction(ctx context.Context, p Principal, eventType, resourceType, resourceID string, detail map[string]any) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	detail["actor_id"] = p.UserID
	detail["result"] = "SUCCESS"
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", eventType, resourceType, resourceID, detail); err != nil {
		return err
	}
	return tx.Commit()
}

type ReversalInput struct {
	Amount       string `json:"amount"`
	ReasonCode   string `json:"reason_code"`
	ReasonDetail string `json:"reason_detail"`
}

func (s *Service) ReverseAllocation(ctx context.Context, p Principal, key, allocationID string, in ReversalInput) (string, error) {
	if textTooLong(in.ReasonCode, 64) || textTooLong(in.ReasonDetail, 500) {
		return "", fmt.Errorf("%w: reversal reason exceeds the length limit", ErrInvalid)
	}
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
	var receiptUnallocated, receiptAmount, receivableReceived, receivableOpen string
	if err = tx.QueryRowContext(ctx, `SELECT unallocated_amount,amount FROM settlement_receipt WHERE id=? AND tenant_id=? FOR UPDATE`, receiptID, p.TenantID).Scan(&receiptUnallocated, &receiptAmount); err != nil {
		return "", ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT received_allocated_amount,open_amount FROM settlement_receivable WHERE id=? AND tenant_id=? FOR UPDATE`, receivableID, p.TenantID).Scan(&receivableReceived, &receivableOpen); err != nil {
		return "", ErrConflict
	}
	// 只有离开 SETTLED 的转换才有结清事实可撤销。
	// 对已部分结清期间的冲销不得再次补偿
	// 较早轮次。
	settledBeforeReversal := add(receivableOpen).Sign() == 0
	receiptAfter := new(big.Rat).Add(add(receiptUnallocated), add(value))
	receiptStatus := "PARTIALLY_ALLOCATED"
	if receiptAfter.Cmp(add(receiptAmount)) == 0 {
		receiptStatus = "AVAILABLE"
	}
	receivableReceivedAfter := new(big.Rat).Sub(add(receivableReceived), add(value))
	if receivableReceivedAfter.Sign() < 0 {
		return "", ErrConflict
	}
	receivableOpenAfter := new(big.Rat).Add(add(receivableOpen), add(value))
	receivableStatus := "PARTIALLY_SETTLED"
	if receivableReceivedAfter.Sign() == 0 {
		receivableStatus = "UNPAID"
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receipt SET unallocated_amount=?,status=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, receiptAfter.FloatString(2), receiptStatus, receiptID, p.TenantID)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET received_allocated_amount=?,open_amount=?,collection_status=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, receivableReceivedAfter.FloatString(2), receivableOpenAfter.FloatString(2), receivableStatus, receivableID, p.TenantID)
	if err != nil {
		return "", err
	}
	var reversalCreditEventID string
	if settledBeforeReversal {
		reversalCreditEventID, err = enqueueCreditReversal(ctx, tx, p.TenantID, allocationID, receivableID)
		if err != nil {
			return "", err
		}
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_ALLOCATION_REVERSED", "receipt_allocation", allocationID, map[string]any{"actor_id": p.UserID, "reversal_id": id, "reason_code": in.ReasonCode, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if reversalCreditEventID != "" && s.CreditPublisher != nil {
		_ = s.RetryCreditSync(ctx, p.TenantID, reversalCreditEventID)
	}
	return id, nil
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

const DunningActionNoteLimit = 500

// CompleteDunningAction 记录催收员确认。动作仅在该命令成功前
// 保持 PENDING；扫描器在案件关闭时取消待处理
// 动作。版本机制使重试和并发标签页安全。
func (s *Service) CompleteDunningAction(ctx context.Context, p Principal, actionID string, version uint64, note string) error {
	actionID = strings.TrimSpace(actionID)
	note = strings.TrimSpace(note)
	if actionID == "" || version == 0 || len([]rune(note)) > DunningActionNoteLimit {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var recipient, status string
	var currentVersion uint64
	err = tx.QueryRowContext(ctx, `SELECT recipient_user_id,status,version FROM settlement_dunning_action WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, actionID).Scan(&recipient, &status, &currentVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if recipient != p.UserID && !p.Allows("settlement.admin") {
		return ErrForbidden
	}
	if status == "COMPLETED" {
		return tx.Commit()
	}
	if status != "PENDING" || currentVersion != version {
		return ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE settlement_dunning_action SET status='COMPLETED',completed_at=UTC_TIMESTAMP(3),completed_by=?,result_detail=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='PENDING' AND version=?`, p.UserID, note, p.TenantID, actionID, version)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrConflict
	}
	if err := outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_DUNNING_ACTION_COMPLETED", "dunning_action", actionID, map[string]any{"actor_id": p.UserID, "result": "SUCCESS", "risk_level": "LOW", "note": note}); err != nil {
		return err
	}
	return tx.Commit()
}

type DunningCoveragePolicy struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Version       int    `json:"version"`
	AgingFromDays int    `json:"aging_from_days"`
	AgingToDays   int    `json:"aging_to_days"`
}

type DunningCoverageGap struct {
	FromDays  int  `json:"from_days"`
	ToDays    int  `json:"to_days,omitempty"`
	OpenEnded bool `json:"open_ended"`
}

type DunningCoverage struct {
	Policies []DunningCoveragePolicy `json:"policies"`
	Gaps     []DunningCoverageGap    `json:"gaps"`
}

// DunningCoverage 报告已启用策略的账龄覆盖缺口，不创建或
// 修改策略。扫描器绝不为缺口臆造接收者或规则；
// 必须由操作员决定业务负责人及动作。
func (s *Service) DunningCoverage(ctx context.Context, p Principal) (DunningCoverage, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,version,aging_from_days,aging_to_days FROM settlement_dunning_policy WHERE tenant_id=? AND enabled=TRUE ORDER BY aging_from_days,aging_to_days,version,id`, p.TenantID)
	if err != nil {
		return DunningCoverage{}, err
	}
	defer rows.Close()
	out := DunningCoverage{Policies: []DunningCoveragePolicy{}, Gaps: []DunningCoverageGap{}}
	for rows.Next() {
		var policy DunningCoveragePolicy
		if err := rows.Scan(&policy.ID, &policy.Name, &policy.Version, &policy.AgingFromDays, &policy.AgingToDays); err != nil {
			return DunningCoverage{}, err
		}
		out.Policies = append(out.Policies, policy)
	}
	if err := rows.Err(); err != nil {
		return DunningCoverage{}, err
	}
	out.Gaps = dunningCoverageGaps(out.Policies)
	return out, nil
}

func dunningCoverageGaps(policies []DunningCoveragePolicy) []DunningCoverageGap {
	gaps := []DunningCoverageGap{}
	cursor := 1
	for _, policy := range policies {
		from, to := policy.AgingFromDays, policy.AgingToDays
		if from < 0 || to < from {
			continue
		}
		if from > cursor {
			gaps = append(gaps, DunningCoverageGap{FromDays: cursor, ToDays: from - 1})
		}
		if to >= cursor {
			cursor = to + 1
		}
	}
	gaps = append(gaps, DunningCoverageGap{FromDays: cursor, OpenEnded: true})
	return gaps
}

type DunningCurrencyTotal struct {
	Currency string `json:"currency"`
	Overdue  string `json:"overdue"`
}

type OutboxEvent struct {
	ID               string `json:"id"`
	EventID          string `json:"event_id"`
	Destination      string `json:"destination"`
	EventType        string `json:"event_type"`
	Status           string `json:"status"`
	AttemptCount     int    `json:"attempt_count"`
	AvailableAt      string `json:"available_at"`
	LastErrorCode    string `json:"last_error_code"`
	LastErrorSummary string `json:"last_error_summary"`
	DeadLetteredAt   string `json:"dead_lettered_at,omitempty"`
	CreatedAt        string `json:"created_at"`
}

// ListOutboxEvents 为受控运维暴露失败或死信事件。
// 只有结算管理员可以查看其他用户的投递失败。
func (s *Service) ListOutboxEvents(ctx context.Context, p Principal) ([]OutboxEvent, error) {
	if !p.Allows("settlement.admin") {
		return nil, ErrForbidden
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,event_id,destination,event_type,status,attempt_count,
		DATE_FORMAT(available_at,'%Y-%m-%dT%H:%i:%s.%fZ'),last_error_code,last_error_summary,
		COALESCE(DATE_FORMAT(dead_lettered_at,'%Y-%m-%dT%H:%i:%s.%fZ'),''),DATE_FORMAT(created_at,'%Y-%m-%dT%H:%i:%s.%fZ')
		FROM settlement_outbox_event WHERE tenant_id=? AND status IN ('RETRY_WAIT','DEAD_LETTER')
		ORDER BY created_at DESC,id DESC LIMIT 200`, p.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxEvent{}
	for rows.Next() {
		var item OutboxEvent
		if err := rows.Scan(&item.ID, &item.EventID, &item.Destination, &item.EventType, &item.Status, &item.AttemptCount, &item.AvailableAt, &item.LastErrorCode, &item.LastErrorSummary, &item.DeadLetteredAt, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// RetryOutboxEvent 在显式管理员命令下重置一条死信
// 事件，绝不编辑载荷或事件 ID，以保留投递身份。
func (s *Service) RetryOutboxEvent(ctx context.Context, p Principal, id string) error {
	if !p.Allows("settlement.admin") || strings.TrimSpace(id) == "" {
		return ErrForbidden
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM settlement_outbox_event WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if status != "DEAD_LETTER" {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE settlement_outbox_event SET status='PENDING',attempt_count=0,available_at=UTC_TIMESTAMP(3),locked_by=NULL,locked_until=NULL,last_error_code='',last_error_summary='',dead_lettered_at=NULL,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='DEAD_LETTER'`, p.TenantID, id); err != nil {
		return err
	}
	if err := outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_OUTBOX_MANUAL_RETRY", "outbox_event", id, map[string]any{"actor_id": p.UserID, "result": "SUCCESS", "risk_level": "MEDIUM"}); err != nil {
		return err
	}
	return tx.Commit()
}

type AgingBucket struct {
	Currency string `json:"currency"`
	Bucket   string `json:"bucket"`
	Amount   string `json:"amount"`
	Count    int64  `json:"count"`
}

// AgingSummary 在 SQL 中按币种和账龄段聚合每条未结清应收，
// 绝不使用浏览器当前加载页。
func (s *Service) AgingSummary(ctx context.Context, p Principal) ([]AgingBucket, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT currency,
		CASE WHEN DATEDIFF(CURDATE(),due_date)<=30 THEN '0-30'
		     WHEN DATEDIFF(CURDATE(),due_date)<=60 THEN '31-60'
		     WHEN DATEDIFF(CURDATE(),due_date)<=90 THEN '61-90'
		     ELSE '90+' END AS bucket,
		COALESCE(SUM(open_amount),0) AS amount, COUNT(*) AS count
		FROM settlement_receivable WHERE tenant_id=? AND open_amount>0
		GROUP BY currency,bucket
		ORDER BY currency, FIELD(bucket,'0-30','31-60','61-90','90+')`, p.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgingBucket{}
	for rows.Next() {
		var item AgingBucket
		if err := rows.Scan(&item.Currency, &item.Bucket, &item.Amount, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type DunningOverview struct {
	OverdueCount       int64                  `json:"overdue_count"`
	OverdueAmount      string                 `json:"overdue_amount"`
	CurrencyTotals     []DunningCurrencyTotal `json:"currency_totals"`
	ActiveCaseCount    int64                  `json:"active_case_count"`
	EscalatedCaseCount int64                  `json:"escalated_case_count"`
	ActionCount        int64                  `json:"action_count"`
	PendingActionCount int64                  `json:"pending_action_count"`
	UncoveredOverdue   int64                  `json:"uncovered_overdue_count"`
}

// DunningOverview 是催收页面使用的服务端聚合，确保
// KPI 卡片不会静默地只描述当前加载页。
func (s *Service) DunningOverview(ctx context.Context, p Principal) (DunningOverview, error) {
	var out DunningOverview
	err := s.DB.QueryRowContext(ctx, `SELECT
COALESCE(SUM(CASE WHEN r.open_amount>0 AND r.due_date<CURDATE() THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN r.open_amount>0 AND r.due_date<CURDATE() THEN r.open_amount ELSE 0 END),0),
(SELECT COUNT(*) FROM settlement_dunning_case dc WHERE dc.tenant_id=? AND dc.status='ACTIVE'),
(SELECT COUNT(*) FROM settlement_dunning_case dc WHERE dc.tenant_id=? AND dc.status='ACTIVE' AND dc.current_escalation_level>1),
(SELECT COUNT(*) FROM settlement_dunning_action da WHERE da.tenant_id=?),
(SELECT COUNT(*) FROM settlement_dunning_action da WHERE da.tenant_id=? AND da.status='PENDING'),
(SELECT COUNT(*) FROM settlement_receivable r2 WHERE r2.tenant_id=? AND r2.open_amount>0 AND r2.due_date<CURDATE() AND NOT EXISTS (
  SELECT 1 FROM settlement_dunning_policy p2 WHERE p2.tenant_id=r2.tenant_id AND p2.enabled=TRUE
    AND DATEDIFF(CURDATE(),r2.due_date) BETWEEN p2.aging_from_days AND p2.aging_to_days))
FROM settlement_receivable r WHERE r.tenant_id=?`, p.TenantID, p.TenantID, p.TenantID, p.TenantID, p.TenantID, p.TenantID).Scan(&out.OverdueCount, &out.OverdueAmount, &out.ActiveCaseCount, &out.EscalatedCaseCount, &out.ActionCount, &out.PendingActionCount, &out.UncoveredOverdue)
	if err != nil {
		return out, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT currency,COALESCE(SUM(open_amount),0) FROM settlement_receivable WHERE tenant_id=? AND open_amount>0 AND due_date<CURDATE() GROUP BY currency ORDER BY currency`, p.TenantID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.CurrencyTotals = []DunningCurrencyTotal{}
	for rows.Next() {
		var item DunningCurrencyTotal
		if err := rows.Scan(&item.Currency, &item.Overdue); err != nil {
			return out, err
		}
		out.CurrencyTotals = append(out.CurrencyTotals, item)
	}
	return out, rows.Err()
}

func (s *Service) CreateDunningPolicy(ctx context.Context, p Principal, in DunningPolicyInput) (string, error) {
	if textTooLong(in.Name, 255) {
		return "", fmt.Errorf("%w: policy name exceeds the length limit", ErrInvalid)
	}
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
