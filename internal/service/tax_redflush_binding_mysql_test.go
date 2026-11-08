package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// seedRedFlushBinding 建立已开票且待税控红冲的真实外键关系。
// 参数：t为测试上下文，db为独占数据库，tenant为合成租户。
// 返回值：红冲申请、尝试、原票与应收标识。
func seedRedFlushBinding(t *testing.T, db *sql.DB, tenant string) (string, string, string, string) {
	t.Helper()
	invoice := seedArchiveInvoice(t, db, tenant)
	var request string
	if err := db.QueryRow(`SELECT invoice_request_id FROM settlement_tax_invoice WHERE id=?`, invoice).Scan(&request); err != nil {
		t.Fatal(err)
	}
	receivable := seedDifferenceReceivable(t, db, tenant)
	steps := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO settlement_invoice_request_item(id,tenant_id,invoice_request_id,line_no,item_name,tax_classification_code,specification,unit,quantity,unit_price_excl_tax,amount_excl_tax,tax_rate,tax_amount,amount_incl_tax) VALUES(?,?,?,1,'服务','','','',1,1,1,0,0,1)`, []any{newID(), tenant, request}},
		{`INSERT INTO settlement_invoice_request_allocation(id,tenant_id,invoice_request_id,receivable_id,reserved_amount,invoiced_amount,status) VALUES(?,?,?,?,1,1,'INVOICED')`, []any{newID(), tenant, request, receivable}},
		{`UPDATE settlement_receivable SET invoiced_amount=1,invoice_status='PARTIALLY_INVOICED' WHERE id=?`, []any{receivable}},
	}
	for _, step := range steps {
		if _, err := db.Exec(step.query, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	svc := &Service{DB: db, InvoiceIssuanceMode: "tax_adapter"}
	id, err := svc.RequestInvoiceRedFlush(t.Context(), Principal{TenantID: tenant, UserID: "applicant"}, newID(), invoice, InvoiceRedFlushInput{ReasonCode: "INVOICE_ERROR", ReasonDetail: "隔离红冲测试"})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.ReviewInvoiceRedFlush(t.Context(), Principal{TenantID: tenant, UserID: "reviewer"}, id, true, InvoiceRedFlushReviewInput{Version: 1}); err != nil {
		t.Fatal(err)
	}
	var attempt string
	if err = db.QueryRow(`SELECT id FROM settlement_invoice_red_flush_attempt WHERE red_flush_request_id=?`, id).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	return id, attempt, invoice, receivable
}

// redFlushBindingState 读取A/B红冲的状态、金额与关系快照用于无副作用比较。
// 参数：t为测试上下文，db为独占库，tenant为合成租户。
// 返回值：按主键排序的关键财务事实JSON。
func redFlushBindingState(t *testing.T, db *sql.DB, tenant string) string {
	t.Helper()
	result := [][][]any{}
	for _, query := range []string{
		`SELECT id,status,version FROM settlement_invoice_red_flush_request WHERE tenant_id=? ORDER BY id`,
		`SELECT id,red_flush_request_id,original_invoice_id,status,result_sequence,red_invoice_id FROM settlement_invoice_red_flush_attempt WHERE tenant_id=? ORDER BY id`,
		`SELECT id,status,amount_excl_tax,tax_amount,amount_incl_tax FROM settlement_tax_invoice WHERE tenant_id=? ORDER BY id`,
		`SELECT id,invoiced_amount,open_amount FROM settlement_receivable WHERE tenant_id=? ORDER BY id`,
		`SELECT red_flush_request_id,original_invoice_id,red_invoice_id FROM settlement_invoice_red_flush_relation WHERE tenant_id=? ORDER BY id`,
		`SELECT red_flush_request_id,original_allocation_id,reversed_amount FROM settlement_invoice_allocation_reversal WHERE tenant_id=? ORDER BY id`,
		`SELECT event_id,processing_status FROM settlement_tax_result_inbox WHERE tenant_id=? ORDER BY id`,
	} {
		rows, err := db.Query(query, tenant)
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		values := [][]any{}
		for rows.Next() {
			row := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range row {
				dest[i] = &row[i]
			}
			if err = rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			values = append(values, row)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		result = append(result, values)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestMySQLRedFlushCallbackBindsAuthoritativeRequest 验证错配回调全量回滚与合法成功幂等。
// 参数：t为测试上下文，仅使用显式隔离DSN。
// 返回值：A尝试与B申请串写、财务事实变化或合法重放失败时测试失败。
func TestMySQLRedFlushCallbackBindsAuthoritativeRequest(t *testing.T) {
	db := refundFinanceMySQL(t)
	for i, eventType := range []string{TaxInvoiceRedFlushedEvent, TaxInvoiceRedFlushUnknownEvent, TaxInvoiceRedFlushFailedEvent} {
		t.Run(eventType, func(t *testing.T) {
			tenant := newID()
			requestA, attemptA, invoiceA, receivableA := seedRedFlushBinding(t, db, tenant)
			requestB, _, _, _ := seedRedFlushBinding(t, db, tenant)
			event := TaxCallbackEvent{EventID: newID(), EventType: eventType, TenantID: tenant, ProviderCode: (&Service{}).taxProviderCode(), OccurredAt: time.Now().UTC(), ResultSequence: 1, RedFlushAttemptID: attemptA, RedFlushRequestID: requestB, ExternalRequestID: attemptA, FailureCode: "PROVIDER_REJECTED"}
			event.Invoice = &TaxInvoiceResult{ExternalInvoiceID: newID(), InvoiceCode: "RED", InvoiceNo: newID(), InvoiceType: "VAT_NORMAL", Currency: "CNY", AmountExclTax: "1", TaxAmount: "0", AmountInclTax: "1", IssueDate: "2026-11-08", Items: []InvoiceItemInput{{ItemName: "服务", Quantity: "1", UnitPriceExclTax: "1", AmountExclTax: "1", TaxRate: "0", TaxAmount: "0", AmountInclTax: "1"}}}
			before := redFlushBindingState(t, db, tenant)
			svc := &Service{DB: db}
			if _, err := svc.ApplyTaxCallback(t.Context(), tenant, event); !errors.Is(err, ErrConflict) {
				t.Errorf("错配回调必须拒绝: %v", err)
			}
			if after := redFlushBindingState(t, db, tenant); after != before {
				t.Errorf("错配回调改变A/B状态金额或关系: before=%s after=%s", before, after)
			}
			if t.Failed() {
				return
			}
			event.EventID = newID()
			event.EventType = TaxInvoiceRedFlushedEvent
			event.RedFlushRequestID = requestA
			if i == 1 {
				event.RedFlushRequestID = ""
			}
			for replay := 0; replay < 2; replay++ {
				if result, err := svc.ApplyTaxCallback(t.Context(), tenant, event); err != nil || result != "APPLIED" {
					t.Fatalf("合法红冲/重放失败: %s %v", result, err)
				}
			}
			var status, amount, relation string
			if err := db.QueryRow(`SELECT status FROM settlement_tax_invoice WHERE id=?`, invoiceA).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT invoiced_amount FROM settlement_receivable WHERE id=?`, receivableA).Scan(&amount); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT red_flush_request_id FROM settlement_invoice_red_flush_relation WHERE original_invoice_id=?`, invoiceA).Scan(&relation); err != nil {
				t.Fatal(err)
			}
			if status != "RED_FLUSHED" || amount != "0.00" || relation != requestA {
				t.Fatal(fmt.Sprintf("合法红冲事实不符: %s/%s/%s", status, amount, relation))
			}
		})
	}
}
