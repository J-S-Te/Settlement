package service

import (
	"crypto/sha256"
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// issueCallbackEvent 构造通过身份校验的开票结果回执，供作废后迟到回调用例复用。
// 参数：eventID 为事件标识，sequence 为回执序号，eventType 为回执类型。
// 返回值：可直接提交给 ApplyTaxCallback 的事件；成功回执携带与申请一致的发票事实。
func issueCallbackEvent(eventID string, sequence uint64, eventType string) TaxCallbackEvent {
	event := TaxCallbackEvent{
		EventID: eventID, EventType: eventType,
		TenantID: "tenant", ProviderCode: "tax-provider",
		OccurredAt: time.Now().UTC(), ResultSequence: sequence,
		AttemptID: "attempt-1", ExternalRequestID: "external-request-1",
	}
	if eventType == TaxInvoiceIssuedEvent {
		event.Invoice = &TaxInvoiceResult{
			ExternalInvoiceID: "external-invoice-1", InvoiceCode: "CODE-1", InvoiceNo: "NO-1",
			InvoiceType: "VAT_NORMAL", Currency: "CNY",
			AmountExclTax: "100.00", TaxAmount: "13.00", AmountInclTax: "113.00",
			IssueDate: "2026-10-30",
			Items: []InvoiceItemInput{{
				ItemName: "服务", Quantity: "1", UnitPriceExclTax: "100.00", AmountExclTax: "100.00",
				TaxRate: "0.13", TaxAmount: "13.00", AmountInclTax: "113.00",
			}},
		}
	}
	return event
}

// issueAttemptRow 返回开票回执主查询的显式列值，顺序必须与 SELECT 列表一致。
// 参数：attemptStatus 为尝试状态，requestStatus 为申请状态，sequence 为已记录序号。
// 返回值：驱动行值。
func issueAttemptRow(attemptStatus, requestStatus string, sequence int64) []driver.Value {
	return []driver.Value{
		"request-1", attemptStatus, requestStatus, "external-request-1", "tax-provider", sequence,
		"VAT_NORMAL", "CNY", "100.00", "13.00", "113.00", []byte(`{}`),
	}
}

// TestIssueCallbackAfterCancelRecordsProviderConflict 验证作废后的迟到"已开票"
// 回执被幂等受理：保留 inbox 行、登记 PROVIDER_CONFLICT 结果与高风险审计，
// 且不重新生成发票、不再次核销预留或释放额度。
// 参数：t 为测试上下文。
// 返回值：无；返回 409 语义错误、丢失 inbox 处理状态、缺少审计或出现额外
// 发票/额度写入时失败（scriptdb 对未脚本化 SQL 直接判失败）。
func TestIssueCallbackAfterCancelRecordsProviderConflict(t *testing.T) {
	var auditArgs, inboxArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_result_inbox"},
		scriptdb.Step{Kind: "query", SQL: "SELECT ia.invoice_request_id,ia.status,ir.status", Rows: [][]driver.Value{issueAttemptRow("ABANDONED", "CANCELLED", 0)}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_issue_attempt SET result_sequence=?"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: &auditArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_tax_result_inbox SET processing_status=?", Args: &inboxArgs},
		scriptdb.Step{Kind: "commit"},
	)
	result, err := (&Service{DB: db}).ApplyTaxCallback(t.Context(), "tenant", issueCallbackEvent("tax-event-1", 3, TaxInvoiceIssuedEvent))
	if err != nil {
		t.Fatalf("作废后迟到回执必须幂等受理而不是 409: %v", err)
	}
	if result != "PROVIDER_CONFLICT" {
		t.Fatalf("结果=%q，期望 PROVIDER_CONFLICT", result)
	}
	if len(inboxArgs) == 0 || inboxArgs[0].Value != "PROVIDER_CONFLICT" {
		t.Fatalf("inbox 结果行必须保留并标记冲突: %+v", inboxArgs)
	}
	if len(auditArgs) < 8 || auditArgs[4].Value != "SETTLEMENT_TAX_PROVIDER_CONFLICT" {
		t.Fatalf("缺少税控冲突审计: %+v", auditArgs)
	}
	payload, _ := auditArgs[7].Value.([]byte)
	for _, want := range []string{`"risk_level":"HIGH"`, `"processing_status":"PROVIDER_CONFLICT"`, `"invoice_no":"NO-1"`, `"attempt_id":"attempt-1"`} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("冲突审计载荷缺少 %s: %s", want, payload)
		}
	}
}

// TestIssueCallbackAfterCancelIgnoresProviderFailure 验证作废后的迟到"开票失败"
// 回执与作废一致：返回 2xx 语义的终态忽略，不写冲突审计也不改申请/额度。
// 参数：t 为测试上下文。
// 返回值：无；失败回执被当作冲突或产生额外写入时失败。
func TestIssueCallbackAfterCancelIgnoresProviderFailure(t *testing.T) {
	var inboxArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_result_inbox"},
		scriptdb.Step{Kind: "query", SQL: "SELECT ia.invoice_request_id,ia.status,ir.status", Rows: [][]driver.Value{issueAttemptRow("ABANDONED", "CANCELLED", 0)}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_tax_result_inbox SET processing_status=?", Args: &inboxArgs},
		scriptdb.Step{Kind: "commit"},
	)
	event := issueCallbackEvent("tax-event-2", 4, TaxInvoiceIssueFailedEvent)
	event.FailureCode, event.FailureMessage = "PROVIDER_REJECTED", "税控拒绝"
	result, err := (&Service{DB: db}).ApplyTaxCallback(t.Context(), "tenant", event)
	if err != nil {
		t.Fatalf("作废后的失败回执必须幂等受理: %v", err)
	}
	if result != "IGNORED_TERMINAL" {
		t.Fatalf("结果=%q，期望 IGNORED_TERMINAL", result)
	}
	if len(inboxArgs) == 0 || inboxArgs[0].Value != "IGNORED_TERMINAL" {
		t.Fatalf("inbox 结果行必须保留: %+v", inboxArgs)
	}
}

// TestIssueCallbackAfterCancelSequenceIsIdempotent 验证作废后冲突事实只登记一次：
// 已推进结果序号后，同一结果以新 event_id 重复投递按陈旧回执忽略。
// 参数：t 为测试上下文。
// 返回值：无；重复投递再次登记冲突时失败。
func TestIssueCallbackAfterCancelSequenceIsIdempotent(t *testing.T) {
	var inboxArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_result_inbox"},
		// 首次冲突已把 result_sequence 推进到 3，重复投递同样序号必须被判为陈旧。
		scriptdb.Step{Kind: "query", SQL: "SELECT ia.invoice_request_id,ia.status,ir.status", Rows: [][]driver.Value{issueAttemptRow("ABANDONED", "CANCELLED", 3)}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_tax_result_inbox SET processing_status=?", Args: &inboxArgs},
		scriptdb.Step{Kind: "commit"},
	)
	result, err := (&Service{DB: db}).ApplyTaxCallback(t.Context(), "tenant", issueCallbackEvent("tax-event-3", 3, TaxInvoiceIssuedEvent))
	if err != nil {
		t.Fatalf("重复投递不得报错: %v", err)
	}
	if result != "IGNORED_STALE" {
		t.Fatalf("结果=%q，期望 IGNORED_STALE", result)
	}
	if len(inboxArgs) == 0 || inboxArgs[0].Value != "IGNORED_STALE" {
		t.Fatalf("重复投递的 inbox 状态=%+v", inboxArgs)
	}
}

// TestIssueCallbackDuplicateEventIsIdempotent 验证同 event_id 同内容重投直接返回
// 已落库处理状态并提交，不再写审计或改动业务事实。
// 参数：t 为测试上下文。
// 返回值：无；重投被当作新回执或未提交时失败。
func TestIssueCallbackDuplicateEventIsIdempotent(t *testing.T) {
	event := issueCallbackEvent("tax-event-4", 5, TaxInvoiceIssuedEvent)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_result_inbox", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		scriptdb.Step{Kind: "query", SQL: "SELECT payload_hash,processing_status", Rows: [][]driver.Value{{hash[:], "PROVIDER_CONFLICT"}}},
		scriptdb.Step{Kind: "commit"},
	)
	result, err := (&Service{DB: db}).ApplyTaxCallback(t.Context(), "tenant", event)
	if err != nil || result != "PROVIDER_CONFLICT" {
		t.Fatalf("重复回执必须幂等: result=%q err=%v", result, err)
	}
}

// TestIssueCallbackFirstTimeStillApplies 验证正常回执路径不变：首次成功回执仍
// 生成发票、终态、核销与审计，而不是被迟到回执处理误吞。
// 参数：t 为测试上下文。
// 返回值：无；正常开票事实缺失或状态不符时失败。
func TestIssueCallbackFirstTimeStillApplies(t *testing.T) {
	var auditArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_result_inbox"},
		scriptdb.Step{Kind: "query", SQL: "SELECT ia.invoice_request_id,ia.status,ir.status", Rows: [][]driver.Value{issueAttemptRow("PENDING", "ISSUE_PENDING", 0)}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_request_item", Rows: [][]driver.Value{{"服务", "", "", "", "1", "100.00", "100.00", "0.13", "13.00", "113.00"}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_invoice(id,tenant_id,invoice_request_id,invoice_code,invoice_no,invoice_type,currency,amount_excl_tax,tax_amount,amount_incl_tax,issue_date,buyer_profile_snapshot,seller_profile_snapshot,status,issued_by_channel"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_invoice_item"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_issue_attempt SET status='ISSUED'"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_request SET status='ISSUED'"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_request_allocation WHERE tenant_id=? AND invoice_request_id=? AND status='RESERVED'", Rows: [][]driver.Value{{"receivable-1", "113.00"}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_request_allocation SET invoiced_amount=reserved_amount"},
		scriptdb.Step{Kind: "query", SQL: "SELECT invoiced_amount,original_amount FROM settlement_receivable", Rows: [][]driver.Value{{"0.00", "113.00"}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable SET invoiced_amount"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: &auditArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_tax_result_inbox SET processing_status=?"},
		scriptdb.Step{Kind: "commit"},
	)
	result, err := (&Service{DB: db}).ApplyTaxCallback(t.Context(), "tenant", issueCallbackEvent("tax-event-5", 1, TaxInvoiceIssuedEvent))
	if err != nil || result != "APPLIED" {
		t.Fatalf("正常回执必须照常入账: result=%q err=%v", result, err)
	}
	if len(auditArgs) < 8 || auditArgs[4].Value != "SETTLEMENT_INVOICE_ISSUED" {
		t.Fatalf("正常开票审计缺失: %+v", auditArgs)
	}
}
