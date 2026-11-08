package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	taxoutbox "github.com/j-s-te/settlement/internal/outbox"
)

// seedApprovedTaxDispatch 通过正式申请及审批建立可发送的税控命令。
// 参数：t/db为隔离测试上下文和连接，tenant为合成租户。
// 返回值：正式服务和可用于可信回调的尝试身份；创建失败时终止测试。
func seedApprovedTaxDispatch(t *testing.T, db *sql.DB, tenant string) (*Service, TaxCallbackEvent) {
	t.Helper()
	actor := roleFlowPrincipal("settlement_accountant", "applicant", tenant)
	reviewer := roleFlowPrincipal("settlement_reviewer", "reviewer", tenant)
	receivable := seedDifferenceReceivable(t, db, tenant)
	var snapshot string
	if err := db.QueryRow(`SELECT contract_snapshot_id FROM settlement_receivable WHERE id=?`, receivable).
		Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: db, InvoiceIssuanceMode: "tax_adapter"}
	item := InvoiceItemInput{ItemName: "服务", Quantity: "1", UnitPriceExclTax: "100",
		AmountExclTax: "100", TaxRate: "0", TaxAmount: "0", AmountInclTax: "100"}
	request, err := svc.CreateInvoiceRequest(t.Context(), actor, newID(), InvoiceRequestInput{
		ContractSnapshotID: snapshot, InvoiceType: "VAT_NORMAL",
		BuyerProfile: map[string]any{"name": "隔离税控客户"}, Items: []InvoiceItemInput{item},
		Allocations: []InvoiceAllocationInput{{ReceivableID: receivable, ReservedAmount: "100"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := svc.ApproveInvoiceRequest(t.Context(), reviewer, request, 1)
	if err != nil {
		t.Fatal(err)
	}
	event := TaxCallbackEvent{EventID: newID(), EventType: TaxInvoiceIssuedEvent,
		TenantID: tenant, AttemptID: attempt, ResultSequence: 1, OccurredAt: time.Now().UTC(),
		Invoice: &TaxInvoiceResult{ExternalInvoiceID: newID(), InvoiceCode: "CODE", InvoiceNo: newID(),
			InvoiceType: "VAT_NORMAL", Currency: "CNY", AmountExclTax: "100", TaxAmount: "0",
			AmountInclTax: "100", IssueDate: "2026-11-08", Items: []InvoiceItemInput{item}},
	}
	if err = db.QueryRow(`SELECT external_request_id,provider_code FROM settlement_invoice_issue_attempt
	 WHERE id=?`, attempt).Scan(&event.ExternalRequestID, &event.ProviderCode); err != nil {
		t.Fatal(err)
	}
	return svc, event
}

// TestMySQLTaxTerminalCommandDoesNotPoisonBatch 验证回调已终结的重领命令不拖累同行新命令。
// 参数：t为测试上下文；仅使用显式隔离DSN创建独占随机schema。
// 返回值：无；终态回退、未发送同行被计失败或完整ACK未落库时失败。
func TestMySQLTaxTerminalCommandDoesNotPoisonBatch(t *testing.T) {
	db := refundFinanceMySQL(t)
	tenant := newID()
	svc, terminal := seedApprovedTaxDispatch(t, db, tenant)
	_, pending := seedApprovedTaxDispatch(t, db, tenant)
	if result, err := svc.ApplyTaxCallback(t.Context(), tenant, terminal); err != nil || result != "APPLIED" {
		t.Fatalf("真实成功回调失败: result=%s err=%v", result, err)
	}
	// 模拟成功回调已提交，但发送者在保存ACK前中断，旧命令租约已经到期。
	if _, err := db.Exec(`UPDATE settlement_outbox_event SET status='PROCESSING',
	 locked_by='crashed-worker',locked_until=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 MINUTE),
	 created_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 2 MINUTE)
	 WHERE tenant_id=? AND destination='TAX_INVOICE_COMMAND' AND JSON_UNQUOTE(
	 JSON_EXTRACT(payload_json,'$.attempt_id'))=?`, tenant, terminal.AttemptID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"synthetic-token","token_type":"Bearer","expires_in":300,"scope":"tax.write"}`)
			return
		}
		calls.Add(1)
		var body struct {
			Events []struct {
				EventID   string `json:"event_id"`
				AttemptID string `json:"attempt_id"`
			} `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) != 1 ||
			body.Events[0].AttemptID != pending.AttemptID {
			t.Errorf("只应发送未终结命令: events=%+v err=%v", body.Events, err)
			w.WriteHeader(http.StatusBadRequest)
			cancel()
			return
		}
		var status string
		if err := db.QueryRow(`SELECT status FROM settlement_invoice_issue_attempt WHERE id=?`, pending.AttemptID).
			Scan(&status); err != nil || status != "UNKNOWN" {
			t.Errorf("HTTP副作用之前未持久UNKNOWN: status=%s err=%v", status, err)
		}
		fmt.Fprintf(w, `{"code":"OK","data":[{"event_id":%q,"status":"ACCEPTED"}]}`, body.Events[0].EventID)
		cancel()
	}))
	defer server.Close()
	worker := &taxoutbox.Worker{Store: &taxoutbox.Store{DB: db, WorkerID: newID(), Lease: time.Minute},
		HTTP: server.Client(), BatchSize: 50, Destinations: []taxoutbox.Destination{{
			Name: "TAX_INVOICE_COMMAND", Endpoint: server.URL + "/tax",
			TokenEndpoint: server.URL + "/token", Scope: "tax.write",
		}}}
	// 防止缺陷版本一直退避而挂住测试；首次扫描后取消即可检查实际落库。
	budget, stopBudget := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer stopBudget()
	if err := worker.Run(budget); !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("新命令应实际发送一次: calls=%d", calls.Load())
	}
	for _, expected := range []struct{ attempt, state, outbox string }{
		{terminal.AttemptID, "ISSUED", "CANCELLED"}, {pending.AttemptID, "ACCEPTED", "DELIVERED"},
	} {
		var state, outbox string
		var attempts int
		var accepted bool
		err := db.QueryRow(`SELECT a.status,o.status,o.attempt_count,a.accepted_at IS NOT NULL
	 FROM settlement_invoice_issue_attempt a JOIN settlement_outbox_event o
	 ON o.tenant_id=a.tenant_id AND o.event_id=a.command_event_id WHERE a.id=?`, expected.attempt).
			Scan(&state, &outbox, &attempts, &accepted)
		if err != nil || state != expected.state || outbox != expected.outbox || attempts != 0 ||
			accepted != (expected.attempt == pending.AttemptID) {
			t.Errorf("attempt=%s: state=%s outbox=%s attempts=%d accepted=%t err=%v",
				expected.attempt, state, outbox, attempts, accepted, err)
		}
	}
}
