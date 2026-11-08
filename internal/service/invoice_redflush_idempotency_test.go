package service

import (
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// redFlushEvent 构造一条通过身份校验的红冲成功回执，供未知/迟到回调用例复用。
// 参数：sequence 为回执序号。
// 返回值：可直接提交给 ApplyTaxCallback 的事件。
func redFlushEvent(sequence uint64) TaxCallbackEvent {
	return TaxCallbackEvent{
		EventID: "red-event", EventType: TaxInvoiceRedFlushedEvent,
		TenantID: "tenant", ProviderCode: "tax-provider",
		OccurredAt: time.Now().UTC(), ResultSequence: sequence,
		RedFlushAttemptID: "red-attempt-1", RedFlushRequestID: "red-request-1",
		ExternalRequestID: "external-request-1",
		Invoice: &TaxInvoiceResult{
			ExternalInvoiceID: "external-red-1", InvoiceCode: "CODE-1", InvoiceNo: "NO-1",
			InvoiceType: "VAT_NORMAL", Currency: "CNY",
			AmountExclTax: "100.00", TaxAmount: "13.00", AmountInclTax: "113.00",
			IssueDate: "2026-10-30",
			Items: []InvoiceItemInput{{
				ItemName: "服务", Quantity: "1", UnitPriceExclTax: "100.00", AmountExclTax: "100.00",
				TaxRate: "0.13", TaxAmount: "13.00", AmountInclTax: "113.00",
			}},
		},
	}
}

// redFlushAttemptRow 返回红冲回执主查询的显式列值，顺序必须与 SELECT 列表一致。
// 参数：requestStatus 为红冲申请状态，attemptStatus 为尝试状态，originalStatus 为原票状态，sequence 为已记录序号。
// 返回值：驱动行值。
func redFlushAttemptRow(requestStatus, attemptStatus, originalStatus string, sequence int64) []driver.Value {
	return []driver.Value{
		"invoice-original", requestStatus, attemptStatus,
		"INVOICE_ERROR", "税额错误", "request-1", "VAT_NORMAL", "CNY",
		"100.00", "13.00", "113.00", []byte(`{}`), []byte(`{}`),
		"tax-provider", "external-request-1", sequence, originalStatus, "red-request-1",
	}
}

// TestRedFlushRequestCountsUnknownAsInFlight 验证 ISSUE_UNKNOWN 未收敛前不能再申请第二笔红冲。
// 参数：t 测试上下文。
// 返回值：无。
func TestRedFlushRequestCountsUnknownAsInFlight(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		// 幂等键先绑定请求内容，再读取原票状态。
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "query", SQL: "SELECT ti.status,ir.contract_snapshot_id", Rows: [][]driver.Value{{"ISSUED", "snapshot-1"}}},
		// 白名单必须同时包含未收敛的 ISSUE_UNKNOWN，否则未知结果会放行第二笔红冲。
		scriptdb.Step{Kind: "query", SQL: "status IN ('SUBMITTED','ISSUE_PENDING','PROCESSING','ISSUE_UNKNOWN')", Rows: [][]driver.Value{{int64(1)}}},
		scriptdb.Step{Kind: "rollback"},
	)
	_, err := (&Service{DB: db}).RequestInvoiceRedFlush(t.Context(), Principal{TenantID: "tenant", UserID: "requester"}, "red-key", "invoice-1", InvoiceRedFlushInput{ReasonCode: "INVOICE_ERROR", ReasonDetail: "税额错误"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second red flush must be refused while unknown is in flight: %v", err)
	}
}

// TestRedFlushRequestCreatesFirstRequest 验证首笔红冲申请在无在途记录时正常创建。
// 参数：t 测试上下文。
// 返回值：无。
func TestRedFlushRequestCreatesFirstRequest(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		// 幂等键先绑定请求内容，再读取原票状态。
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "query", SQL: "SELECT ti.status,ir.contract_snapshot_id", Rows: [][]driver.Value{{"ISSUED", "snapshot-1"}}},
		scriptdb.Step{Kind: "query", SQL: "status IN ('SUBMITTED','ISSUE_PENDING','PROCESSING','ISSUE_UNKNOWN')", Rows: [][]driver.Value{{int64(0)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_invoice_red_flush_request"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "commit"},
	)
	id, err := (&Service{DB: db}).RequestInvoiceRedFlush(t.Context(), Principal{TenantID: "tenant", UserID: "requester"}, "red-key", "invoice-1", InvoiceRedFlushInput{ReasonCode: "INVOICE_ERROR", ReasonDetail: "税额错误"})
	if err != nil || id == "" {
		t.Fatalf("first red flush must still be created: id=%q err=%v", id, err)
	}
}

// TestRedFlushCallbackIgnoresLateSuccessAfterOriginalRedFlushed 验证原票已红冲后的迟到成功回执不生成负票。
// 参数：t 测试上下文。
// 返回值：无。
func TestRedFlushCallbackIgnoresLateSuccessAfterOriginalRedFlushed(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_result_inbox"},
		// 原票已 RED_FLUSHED：只允许读取，不得插入负票、红冲关系或再次冲减。
		scriptdb.Step{Kind: "query", SQL: "SELECT ra.original_invoice_id,rf.status,ra.status", Rows: [][]driver.Value{redFlushAttemptRow("ISSUE_PENDING", "PENDING", "RED_FLUSHED", 0)}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_tax_result_inbox SET processing_status"},
		scriptdb.Step{Kind: "commit"},
	)
	result, err := (&Service{DB: db}).ApplyTaxCallback(t.Context(), "tenant", redFlushEvent(1))
	if err != nil || result != "IGNORED_TERMINAL" {
		t.Fatalf("late success callback must be ignored: result=%q err=%v", result, err)
	}
}

// TestRedFlushCallbackAppliesFirstTime 验证首笔红冲成功回执仍生成负票、冲销与审计。
// 参数：t 测试上下文。
// 返回值：无。
func TestRedFlushCallbackAppliesFirstTime(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_result_inbox"},
		scriptdb.Step{Kind: "query", SQL: "SELECT ra.original_invoice_id,rf.status,ra.status", Rows: [][]driver.Value{redFlushAttemptRow("ISSUE_PENDING", "PENDING", "ISSUED", 0)}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_request_item", Rows: [][]driver.Value{{"服务", "", "", "", "1", "100.00", "100.00", "0.13", "13.00", "113.00"}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_invoice(id,tenant_id,invoice_request_id,invoice_code,invoice_no,invoice_type,currency,amount_excl_tax,tax_amount,amount_incl_tax,issue_date,buyer_profile_snapshot,seller_profile_snapshot,status,issued_by_channel"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_invoice_item"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_invoice_red_flush_relation"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_tax_invoice SET status='RED_FLUSHED'"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_red_flush_request SET status='RED_FLUSHED'"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_red_flush_attempt SET status='ISSUED'"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_request_allocation WHERE tenant_id=? AND invoice_request_id=? AND status='INVOICED'", Rows: [][]driver.Value{{"allocation-1", "receivable-1", "113.00"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_invoice_allocation_reversal"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_invoice_allocation_reversal"},
		scriptdb.Step{Kind: "query", SQL: "SELECT invoiced_amount,original_amount FROM settlement_receivable", Rows: [][]driver.Value{{"113.00", "113.00"}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable SET invoiced_amount"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_tax_result_inbox SET processing_status"},
		scriptdb.Step{Kind: "commit"},
	)
	result, err := (&Service{DB: db}).ApplyTaxCallback(t.Context(), "tenant", redFlushEvent(1))
	if err != nil || result != "APPLIED" {
		t.Fatalf("first red flush must still apply: result=%q err=%v", result, err)
	}
}

// TestReverseInvoiceAllocationsSkipsAlreadyReversedAllocation 验证同一核销不会被二次冲减。
// 参数：t 测试上下文。
// 返回值：无。
func TestReverseInvoiceAllocationsSkipsAlreadyReversedAllocation(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_request_allocation WHERE tenant_id=? AND invoice_request_id=? AND status='INVOICED'", Rows: [][]driver.Value{{"allocation-1", "receivable-1", "113.00"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_invoice_allocation_reversal", Rows: [][]driver.Value{{"reversal-existing"}}},
		scriptdb.Step{Kind: "commit"},
	)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// 步表只允许 begin/查询/commit：任何 INSERT 或已开票额递减都会让用例失败。
	if err = reverseInvoiceAllocations(t.Context(), tx, "tenant", "red-request-2", "request-1"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestReverseInvoiceAllocationsReversesFirstTime 验证首次冲销仍写入冲销记录并递减已开票额。
// 参数：t 测试上下文。
// 返回值：无。
func TestReverseInvoiceAllocationsReversesFirstTime(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_request_allocation WHERE tenant_id=? AND invoice_request_id=? AND status='INVOICED'", Rows: [][]driver.Value{{"allocation-1", "receivable-1", "113.00"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_invoice_allocation_reversal"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_invoice_allocation_reversal"},
		scriptdb.Step{Kind: "query", SQL: "SELECT invoiced_amount,original_amount FROM settlement_receivable", Rows: [][]driver.Value{{"113.00", "113.00"}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable SET invoiced_amount"},
		scriptdb.Step{Kind: "commit"},
	)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = reverseInvoiceAllocations(t.Context(), tx, "tenant", "red-request-1", "request-1"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
