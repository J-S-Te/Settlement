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

const (
	TaxInvoiceIssuedEvent          = "tax.invoice.issued.v1"
	TaxInvoiceIssueFailedEvent     = "tax.invoice.issue_failed.v1"
	TaxInvoiceIssueUnknownEvent    = "tax.invoice.issue_unknown.v1"
	TaxInvoiceRedFlushedEvent      = "tax.invoice.red_flushed.v1"
	TaxInvoiceRedFlushFailedEvent  = "tax.invoice.red_flush_failed.v1"
	TaxInvoiceRedFlushUnknownEvent = "tax.invoice.red_flush_unknown.v1"
)

type TaxInvoiceResult struct {
	ExternalInvoiceID string             `json:"external_invoice_id"`
	InvoiceCode       string             `json:"invoice_code"`
	InvoiceNo         string             `json:"invoice_no"`
	InvoiceType       string             `json:"invoice_type"`
	Currency          string             `json:"currency"`
	AmountExclTax     string             `json:"amount_excl_tax"`
	TaxAmount         string             `json:"tax_amount"`
	AmountInclTax     string             `json:"amount_incl_tax"`
	IssueDate         string             `json:"issue_date"`
	SellerProfile     map[string]any     `json:"seller_profile"`
	Items             []InvoiceItemInput `json:"items"`
}

type TaxCallbackEvent struct {
	EventID             string            `json:"event_id"`
	EventType           string            `json:"event_type"`
	TenantID            string            `json:"tenant_id"`
	ProviderCode        string            `json:"provider_code"`
	OccurredAt          time.Time         `json:"occurred_at"`
	ResultSequence      uint64            `json:"result_sequence"`
	AttemptID           string            `json:"attempt_id"`
	RedFlushAttemptID   string            `json:"red_flush_attempt_id"`
	RedFlushRequestID   string            `json:"red_flush_request_id"`
	ExternalRequestID   string            `json:"external_request_id"`
	ExternalOperationID string            `json:"external_operation_id"`
	FailureCode         string            `json:"failure_code"`
	FailureMessage      string            `json:"failure_message"`
	Invoice             *TaxInvoiceResult `json:"invoice,omitempty"`
}

func (s *Service) ApplyTaxCallback(ctx context.Context, trustedTenant string, event TaxCallbackEvent) (string, error) {
	trustedTenant = strings.TrimSpace(trustedTenant)
	event.ProviderCode = strings.TrimSpace(event.ProviderCode)
	if trustedTenant == "" || event.TenantID != trustedTenant || event.ProviderCode == "" || len(event.ProviderCode) > 64 || len(event.EventID) < 1 || len(event.EventID) > 128 || event.ResultSequence == 0 || event.OccurredAt.IsZero() || event.OccurredAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return "", fmt.Errorf("%w: invalid tax callback identity or event metadata", ErrInvalid)
	}
	invoiceEvent := event.EventType == TaxInvoiceIssuedEvent || event.EventType == TaxInvoiceIssueFailedEvent || event.EventType == TaxInvoiceIssueUnknownEvent
	redFlushEvent := event.EventType == TaxInvoiceRedFlushedEvent || event.EventType == TaxInvoiceRedFlushFailedEvent || event.EventType == TaxInvoiceRedFlushUnknownEvent
	if !invoiceEvent && !redFlushEvent {
		return "", fmt.Errorf("%w: unsupported tax callback event_type", ErrInvalid)
	}
	operationType, aggregateID := "INVOICE_ISSUE", strings.TrimSpace(event.AttemptID)
	if redFlushEvent {
		operationType, aggregateID = "RED_FLUSH", strings.TrimSpace(event.RedFlushAttemptID)
	}
	if aggregateID == "" || len(aggregateID) > 32 {
		return "", fmt.Errorf("%w: callback aggregate id is required", ErrInvalid)
	}
	if len(event.ExternalRequestID) > 128 || len(event.FailureCode) > 64 || len(event.FailureMessage) > 500 {
		return "", fmt.Errorf("%w: callback field is too long", ErrInvalid)
	}
	success := event.EventType == TaxInvoiceIssuedEvent || event.EventType == TaxInvoiceRedFlushedEvent
	if success {
		if err := validateTaxInvoiceResult(event.Invoice); err != nil {
			return "", err
		}
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	payloadHash := sha256.Sum256(payload)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	callbackID := newID()
	resultStatus := "SUCCEEDED"
	if strings.Contains(event.EventType, "unknown") {
		resultStatus = "UNKNOWN"
	} else if strings.Contains(event.EventType, "failed") {
		resultStatus = "REJECTED"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_tax_result_inbox(id,tenant_id,provider_code,event_id,event_type,operation_type,aggregate_id,external_request_id,external_operation_id,result_sequence,result_status,payload_json,payload_hash,processing_status,failure_code,failure_summary,occurred_at,received_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'PROCESSING',?,?,?,UTC_TIMESTAMP(3))`, callbackID, trustedTenant, event.ProviderCode, event.EventID, event.EventType, operationType, aggregateID, event.ExternalRequestID, nullable(event.ExternalOperationID), event.ResultSequence, resultStatus, payload, payloadHash[:], event.FailureCode, event.FailureMessage, event.OccurredAt.UTC())
	if err != nil {
		if !mysqlDuplicate(err) {
			return "", err
		}
		var storedHash []byte
		var storedResult string
		if err = tx.QueryRowContext(ctx, `SELECT payload_hash,processing_status FROM settlement_tax_result_inbox WHERE tenant_id=? AND provider_code=? AND event_id=?`, trustedTenant, event.ProviderCode, event.EventID).Scan(&storedHash, &storedResult); err != nil {
			return "", err
		}
		if !bytes.Equal(storedHash, payloadHash[:]) {
			return "", fmt.Errorf("%w: callback event_id was reused with different content", ErrConflict)
		}
		return storedResult, tx.Commit()
	}
	result := "APPLIED"
	if invoiceEvent {
		result, err = s.applyInvoiceIssueCallback(ctx, tx, trustedTenant, event)
	} else {
		result, err = s.applyRedFlushCallback(ctx, tx, trustedTenant, event)
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_tax_result_inbox SET processing_status=?,processed_at=UTC_TIMESTAMP(3) WHERE id=?`, result, callbackID); err != nil {
		return "", err
	}
	return result, tx.Commit()
}

func validateTaxInvoiceResult(invoice *TaxInvoiceResult) error {
	if invoice == nil {
		return fmt.Errorf("%w: invoice result is required", ErrInvalid)
	}
	for name, value := range map[string]string{"external_invoice_id": invoice.ExternalInvoiceID, "invoice_code": invoice.InvoiceCode, "invoice_no": invoice.InvoiceNo, "invoice_type": invoice.InvoiceType, "currency": invoice.Currency, "amount_excl_tax": invoice.AmountExclTax, "tax_amount": invoice.TaxAmount, "amount_incl_tax": invoice.AmountInclTax, "issue_date": invoice.IssueDate} {
		if _, err := required(value, name); err != nil {
			return err
		}
	}
	if len(invoice.InvoiceCode) > 64 || len(invoice.InvoiceNo) > 64 || len(invoice.ExternalInvoiceID) > 128 {
		return fmt.Errorf("%w: invoice result field is too long", ErrInvalid)
	}
	if _, err := time.Parse("2006-01-02", invoice.IssueDate); err != nil {
		return fmt.Errorf("%w: issue_date must be YYYY-MM-DD", ErrInvalid)
	}
	if len(invoice.Currency) != 3 || len(invoice.Items) == 0 {
		return fmt.Errorf("%w: currency and invoice items are required", ErrInvalid)
	}
	return nil
}

func nullable(value string) any {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return nil
}

func (s *Service) applyInvoiceIssueCallback(ctx context.Context, tx *sql.Tx, tenant string, event TaxCallbackEvent) (string, error) {
	var requestID, attemptStatus, requestStatus, externalRequestID, providerCode, kind, currency, excl, tax, incl string
	var lastSequence uint64
	var buyer []byte
	err := tx.QueryRowContext(ctx, `SELECT ia.invoice_request_id,ia.status,ir.status,ia.external_request_id,ia.provider_code,ia.result_sequence,ir.invoice_type,cs.currency,ir.amount_excl_tax,ir.tax_amount,ir.amount_incl_tax,ir.buyer_profile_snapshot FROM settlement_invoice_issue_attempt ia JOIN settlement_invoice_request ir ON ir.id=ia.invoice_request_id AND ir.tenant_id=ia.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id WHERE ia.id=? AND ia.tenant_id=? AND ia.channel='TAX_ADAPTER' FOR UPDATE`, event.AttemptID, tenant).Scan(&requestID, &attemptStatus, &requestStatus, &externalRequestID, &providerCode, &lastSequence, &kind, &currency, &excl, &tax, &incl, &buyer)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if event.ExternalRequestID != externalRequestID {
		return "", fmt.Errorf("%w: external_request_id does not match attempt", ErrConflict)
	}
	if event.ProviderCode != providerCode {
		return "", fmt.Errorf("%w: provider_code does not match attempt", ErrConflict)
	}
	if event.ResultSequence <= lastSequence {
		return "IGNORED_STALE", nil
	}
	if attemptStatus == "ISSUED" || requestStatus == "ISSUED" {
		return "IGNORED_TERMINAL", nil
	}
	if attemptStatus == "ISSUE_FAILED" || requestStatus == "ISSUE_FAILED" {
		if event.EventType == TaxInvoiceIssuedEvent {
			return "PROVIDER_CONFLICT", nil
		}
		return "IGNORED_TERMINAL", nil
	}
	if attemptStatus != "PENDING" && attemptStatus != "ACCEPTED" && attemptStatus != "PROCESSING" && attemptStatus != "UNKNOWN" {
		return "", ErrConflict
	}
	if event.EventType == TaxInvoiceIssueUnknownEvent {
		_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET status='UNKNOWN',result_sequence=?,last_result_event_id=?,external_operation_id=COALESCE(external_operation_id,?),failure_code=?,failure_message=?,responded_at=UTC_TIMESTAMP(3),last_result_at=UTC_TIMESTAMP(3),unknown_since=COALESCE(unknown_since,UTC_TIMESTAMP(3)),next_reconcile_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 1 MINUTE) WHERE id=? AND tenant_id=?`, event.ResultSequence, event.EventID, nullable(event.ExternalOperationID), event.FailureCode, event.FailureMessage, event.AttemptID, tenant)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='ISSUE_UNKNOWN',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status IN ('ISSUE_PENDING','ISSUE_UNKNOWN')`, requestID, tenant)
		}
		return "APPLIED", err
	}
	if event.EventType == TaxInvoiceIssueFailedEvent {
		if strings.TrimSpace(event.FailureCode) == "" {
			return "", fmt.Errorf("%w: failure_code is required", ErrInvalid)
		}
		_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET status='ISSUE_FAILED',result_sequence=?,last_result_event_id=?,external_operation_id=COALESCE(external_operation_id,?),failure_code=?,failure_message=?,responded_at=UTC_TIMESTAMP(3),last_result_at=UTC_TIMESTAMP(3),next_reconcile_at=NULL WHERE id=? AND tenant_id=?`, event.ResultSequence, event.EventID, nullable(event.ExternalOperationID), event.FailureCode, event.FailureMessage, event.AttemptID, tenant)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='ISSUE_FAILED',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status IN ('ISSUE_PENDING','ISSUE_UNKNOWN')`, requestID, tenant)
		}
		return "APPLIED", err
	}
	if event.Invoice.InvoiceType != kind || !strings.EqualFold(event.Invoice.Currency, currency) || !sameMoney(event.Invoice.AmountExclTax, excl) || !sameMoney(event.Invoice.TaxAmount, tax) || !sameMoney(event.Invoice.AmountInclTax, incl) {
		return "", fmt.Errorf("%w: returned invoice totals do not match request", ErrConflict)
	}
	if err = validateInvoiceResultItems(ctx, tx, tenant, requestID, event.Invoice.Items); err != nil {
		return "", err
	}
	seller, _ := json.Marshal(event.Invoice.SellerProfile)
	invoiceID := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_tax_invoice(id,tenant_id,invoice_request_id,invoice_code,invoice_no,invoice_type,currency,amount_excl_tax,tax_amount,amount_incl_tax,issue_date,buyer_profile_snapshot,seller_profile_snapshot,status,issued_by_channel,provider_code,external_invoice_id,external_payload_hash,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'ISSUED','TAX_ADAPTER',?,?,?,UTC_TIMESTAMP(3))`, invoiceID, tenant, requestID, strings.TrimSpace(event.Invoice.InvoiceCode), strings.TrimSpace(event.Invoice.InvoiceNo), kind, strings.ToUpper(currency), excl, tax, incl, event.Invoice.IssueDate, buyer, seller, event.ProviderCode, event.Invoice.ExternalInvoiceID, invoiceResultHash(event.Invoice))
	if err != nil {
		return "", err
	}
	for line, item := range event.Invoice.Items {
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_tax_invoice_item(id,tenant_id,tax_invoice_id,line_no,item_name,tax_classification_code,specification,unit,quantity,unit_price_excl_tax,amount_excl_tax,tax_rate,tax_amount,amount_incl_tax) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, newID(), tenant, invoiceID, line+1, item.ItemName, item.TaxClassificationCode, item.Specification, item.Unit, item.Quantity, item.UnitPriceExclTax, item.AmountExclTax, item.TaxRate, item.TaxAmount, item.AmountInclTax)
		if err != nil {
			return "", err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET status='ISSUED',external_invoice_id=?,external_operation_id=COALESCE(external_operation_id,?),tax_invoice_id=?,result_sequence=?,last_result_event_id=?,failure_code='',failure_message='',responded_at=UTC_TIMESTAMP(3),last_result_at=UTC_TIMESTAMP(3),next_reconcile_at=NULL WHERE id=? AND tenant_id=?`, event.Invoice.ExternalInvoiceID, nullable(event.ExternalOperationID), invoiceID, event.ResultSequence, event.EventID, event.AttemptID, tenant); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request SET status='ISSUED',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status IN ('ISSUE_PENDING','ISSUE_UNKNOWN')`, requestID, tenant); err != nil {
		return "", err
	}
	if err = finalizeInvoiceAllocations(ctx, tx, tenant, requestID); err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, tenant, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_ISSUED", "tax_invoice", invoiceID, map[string]any{"actor_id": "TAX_ADAPTER", "invoice_request_id": requestID, "attempt_id": event.AttemptID, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return "APPLIED", nil
}

func (s *Service) applyRedFlushCallback(ctx context.Context, tx *sql.Tx, tenant string, event TaxCallbackEvent) (string, error) {
	var originalInvoiceID, status, attemptStatus, reasonCode, reasonDetail, requestID, kind, currency, excl, tax, incl, providerCode, externalRequestID string
	var lastSequence uint64
	var buyer, originalSeller []byte
	err := tx.QueryRowContext(ctx, `SELECT ra.original_invoice_id,rf.status,ra.status,rf.reason_code,rf.reason_detail,ti.invoice_request_id,ti.invoice_type,ti.currency,ti.amount_excl_tax,ti.tax_amount,ti.amount_incl_tax,ti.buyer_profile_snapshot,ti.seller_profile_snapshot,ra.provider_code,ra.external_request_id,ra.result_sequence FROM settlement_invoice_red_flush_attempt ra JOIN settlement_invoice_red_flush_request rf ON rf.id=ra.red_flush_request_id AND rf.tenant_id=ra.tenant_id JOIN settlement_tax_invoice ti ON ti.id=ra.original_invoice_id AND ti.tenant_id=ra.tenant_id WHERE ra.id=? AND ra.tenant_id=? FOR UPDATE`, event.RedFlushAttemptID, tenant).Scan(&originalInvoiceID, &status, &attemptStatus, &reasonCode, &reasonDetail, &requestID, &kind, &currency, &excl, &tax, &incl, &buyer, &originalSeller, &providerCode, &externalRequestID, &lastSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if event.RedFlushRequestID == "" {
		if err = tx.QueryRowContext(ctx, `SELECT red_flush_request_id FROM settlement_invoice_red_flush_attempt WHERE id=? AND tenant_id=?`, event.RedFlushAttemptID, tenant).Scan(&event.RedFlushRequestID); err != nil {
			return "", err
		}
	}
	if event.ExternalRequestID != externalRequestID || event.ProviderCode != providerCode {
		return "", fmt.Errorf("%w: external_request_id does not match red flush request", ErrConflict)
	}
	if event.ResultSequence <= lastSequence {
		return "IGNORED_STALE", nil
	}
	if status == "RED_FLUSHED" {
		return "IGNORED_TERMINAL", nil
	}
	if status == "ISSUE_FAILED" {
		if event.EventType == TaxInvoiceRedFlushedEvent {
			return "PROVIDER_CONFLICT", nil
		}
		return "IGNORED_TERMINAL", nil
	}
	if (status != "ISSUE_PENDING" && status != "ISSUE_UNKNOWN") || (attemptStatus != "PENDING" && attemptStatus != "UNKNOWN" && attemptStatus != "ACCEPTED") {
		return "", ErrConflict
	}
	if event.EventType == TaxInvoiceRedFlushUnknownEvent {
		_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_attempt SET status='UNKNOWN',result_sequence=?,last_result_event_id=?,external_operation_id=COALESCE(external_operation_id,?),failure_code=?,failure_message=?,responded_at=UTC_TIMESTAMP(3),unknown_since=COALESCE(unknown_since,UTC_TIMESTAMP(3)),next_reconcile_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 1 MINUTE) WHERE id=? AND tenant_id=?`, event.ResultSequence, event.EventID, nullable(event.ExternalOperationID), event.FailureCode, event.FailureMessage, event.RedFlushAttemptID, tenant)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_request SET status='ISSUE_UNKNOWN',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, event.RedFlushRequestID, tenant)
		}
		return "APPLIED", err
	}
	if event.EventType == TaxInvoiceRedFlushFailedEvent {
		if strings.TrimSpace(event.FailureCode) == "" {
			return "", fmt.Errorf("%w: failure_code is required", ErrInvalid)
		}
		_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_attempt SET status='ISSUE_FAILED',result_sequence=?,last_result_event_id=?,external_operation_id=COALESCE(external_operation_id,?),failure_code=?,failure_message=?,responded_at=UTC_TIMESTAMP(3),next_reconcile_at=NULL WHERE id=? AND tenant_id=?`, event.ResultSequence, event.EventID, nullable(event.ExternalOperationID), event.FailureCode, event.FailureMessage, event.RedFlushAttemptID, tenant)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_request SET status='ISSUE_FAILED',review_reason=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, strings.TrimSpace(event.FailureCode+": "+event.FailureMessage), event.RedFlushRequestID, tenant)
		}
		return "APPLIED", err
	}
	if event.Invoice.InvoiceType != kind || !strings.EqualFold(event.Invoice.Currency, currency) || !sameMoney(event.Invoice.AmountExclTax, excl) || !sameMoney(event.Invoice.TaxAmount, tax) || !sameMoney(event.Invoice.AmountInclTax, incl) {
		return "", fmt.Errorf("%w: red invoice absolute totals do not match original invoice", ErrConflict)
	}
	if err = validateInvoiceResultItems(ctx, tx, tenant, requestID, event.Invoice.Items); err != nil {
		return "", err
	}
	seller := originalSeller
	if event.Invoice != nil && event.Invoice.SellerProfile != nil {
		seller, _ = json.Marshal(event.Invoice.SellerProfile)
	}
	redInvoiceID := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_tax_invoice(id,tenant_id,invoice_request_id,invoice_code,invoice_no,invoice_type,currency,amount_excl_tax,tax_amount,amount_incl_tax,issue_date,buyer_profile_snapshot,seller_profile_snapshot,status,issued_by_channel,provider_code,external_invoice_id,external_payload_hash,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'ISSUED','TAX_ADAPTER',?,?,?,UTC_TIMESTAMP(3))`, redInvoiceID, tenant, requestID, event.Invoice.InvoiceCode, event.Invoice.InvoiceNo, kind, strings.ToUpper(currency), negateDecimal(excl), negateDecimal(tax), negateDecimal(incl), event.Invoice.IssueDate, buyer, seller, event.ProviderCode, event.Invoice.ExternalInvoiceID, invoiceResultHash(event.Invoice))
	if err != nil {
		return "", err
	}
	for line, item := range event.Invoice.Items {
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_tax_invoice_item(id,tenant_id,tax_invoice_id,line_no,item_name,tax_classification_code,specification,unit,quantity,unit_price_excl_tax,amount_excl_tax,tax_rate,tax_amount,amount_incl_tax) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, newID(), tenant, redInvoiceID, line+1, item.ItemName, item.TaxClassificationCode, item.Specification, item.Unit, item.Quantity, item.UnitPriceExclTax, negateDecimal(item.AmountExclTax), item.TaxRate, negateDecimal(item.TaxAmount), negateDecimal(item.AmountInclTax))
		if err != nil {
			return "", err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_red_flush_relation(id,tenant_id,red_flush_request_id,original_invoice_id,red_invoice_id,reason_code,reason_detail,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3))`, newID(), tenant, event.RedFlushRequestID, originalInvoiceID, redInvoiceID, reasonCode, reasonDetail, "TAX_ADAPTER"); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_tax_invoice SET status='RED_FLUSHED' WHERE id=? AND tenant_id=? AND status='ISSUED'`, originalInvoiceID, tenant); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_request SET status='RED_FLUSHED',version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, event.RedFlushRequestID, tenant); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_attempt SET status='ISSUED',red_invoice_id=?,external_operation_id=COALESCE(external_operation_id,?),result_sequence=?,last_result_event_id=?,responded_at=UTC_TIMESTAMP(3),next_reconcile_at=NULL WHERE id=? AND tenant_id=?`, redInvoiceID, nullable(event.ExternalOperationID), event.ResultSequence, event.EventID, event.RedFlushAttemptID, tenant); err != nil {
		return "", err
	}
	if err = reverseInvoiceAllocations(ctx, tx, tenant, event.RedFlushRequestID, requestID); err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, tenant, "PLATFORM_AUDIT", "SETTLEMENT_INVOICE_RED_FLUSHED", "tax_invoice", originalInvoiceID, map[string]any{"actor_id": "TAX_ADAPTER", "red_invoice_id": redInvoiceID, "red_flush_request_id": event.RedFlushRequestID, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return "APPLIED", nil
}

func negateDecimal(value string) string {
	number, ok := new(big.Rat).SetString(value)
	if !ok {
		return "0.00"
	}
	return number.Neg(number).FloatString(2)
}

func sameMoney(left, right string) bool {
	l, lok := new(big.Rat).SetString(strings.TrimSpace(left))
	r, rok := new(big.Rat).SetString(strings.TrimSpace(right))
	return lok && rok && l.Cmp(r) == 0
}

func invoiceResultHash(invoice *TaxInvoiceResult) []byte {
	payload, _ := json.Marshal(invoice)
	digest := sha256.Sum256(payload)
	return digest[:]
}

func validateInvoiceResultItems(ctx context.Context, tx *sql.Tx, tenant, requestID string, actual []InvoiceItemInput) error {
	rows, err := tx.QueryContext(ctx, `SELECT item_name,tax_classification_code,specification,unit,quantity,unit_price_excl_tax,amount_excl_tax,tax_rate,tax_amount,amount_incl_tax FROM settlement_invoice_request_item WHERE tenant_id=? AND invoice_request_id=? ORDER BY line_no`, tenant, requestID)
	if err != nil {
		return err
	}
	defer rows.Close()
	expected := []InvoiceItemInput{}
	for rows.Next() {
		var item InvoiceItemInput
		if err = rows.Scan(&item.ItemName, &item.TaxClassificationCode, &item.Specification, &item.Unit, &item.Quantity, &item.UnitPriceExclTax, &item.AmountExclTax, &item.TaxRate, &item.TaxAmount, &item.AmountInclTax); err != nil {
			return err
		}
		expected = append(expected, item)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(expected) != len(actual) {
		return fmt.Errorf("%w: returned invoice items do not match request", ErrConflict)
	}
	for index := range expected {
		if expected[index].ItemName != actual[index].ItemName || expected[index].TaxClassificationCode != actual[index].TaxClassificationCode || expected[index].Specification != actual[index].Specification || expected[index].Unit != actual[index].Unit || !sameMoney(expected[index].Quantity, actual[index].Quantity) || !sameMoney(expected[index].UnitPriceExclTax, actual[index].UnitPriceExclTax) || !sameMoney(expected[index].AmountExclTax, actual[index].AmountExclTax) || !sameMoney(expected[index].TaxRate, actual[index].TaxRate) || !sameMoney(expected[index].TaxAmount, actual[index].TaxAmount) || !sameMoney(expected[index].AmountInclTax, actual[index].AmountInclTax) {
			return fmt.Errorf("%w: returned invoice item %d does not match request", ErrConflict, index+1)
		}
	}
	return nil
}

func finalizeInvoiceAllocations(ctx context.Context, tx *sql.Tx, tenant, requestID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT receivable_id,reserved_amount FROM settlement_invoice_request_allocation WHERE tenant_id=? AND invoice_request_id=? AND status='RESERVED' FOR UPDATE`, tenant, requestID)
	if err != nil {
		return err
	}
	type allocation struct{ receivableID, amount string }
	allocations := []allocation{}
	for rows.Next() {
		var item allocation
		if err = rows.Scan(&item.receivableID, &item.amount); err != nil {
			rows.Close()
			return err
		}
		allocations = append(allocations, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range allocations {
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_request_allocation SET invoiced_amount=reserved_amount,status='INVOICED' WHERE tenant_id=? AND invoice_request_id=? AND receivable_id=? AND status='RESERVED'`, tenant, requestID, item.receivableID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET invoiced_amount=invoiced_amount+?,invoice_status=CASE WHEN invoiced_amount+? >= original_amount THEN 'FULLY_INVOICED' ELSE 'PARTIALLY_INVOICED' END,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, item.amount, item.amount, item.receivableID, tenant); err != nil {
			return err
		}
	}
	return nil
}

func reverseInvoiceAllocations(ctx context.Context, tx *sql.Tx, tenant, redFlushRequestID, requestID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,receivable_id,invoiced_amount FROM settlement_invoice_request_allocation WHERE tenant_id=? AND invoice_request_id=? AND status='INVOICED' FOR UPDATE`, tenant, requestID)
	if err != nil {
		return err
	}
	type reversal struct{ allocationID, receivableID, amount string }
	items := []reversal{}
	for rows.Next() {
		var item reversal
		if err = rows.Scan(&item.allocationID, &item.receivableID, &item.amount); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range items {
		if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_allocation_reversal(id,tenant_id,red_flush_request_id,original_allocation_id,reversed_amount,created_at) VALUES(?,?,?,?,?,UTC_TIMESTAMP(3))`, newID(), tenant, redFlushRequestID, item.allocationID, item.amount); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET invoiced_amount=GREATEST(0,invoiced_amount-?),invoice_status=CASE WHEN GREATEST(0,invoiced_amount-?)=0 THEN 'NOT_INVOICED' WHEN GREATEST(0,invoiced_amount-?)<original_amount THEN 'PARTIALLY_INVOICED' ELSE 'FULLY_INVOICED' END,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, item.amount, item.amount, item.amount, item.receivableID, tenant); err != nil {
			return err
		}
	}
	return nil
}
