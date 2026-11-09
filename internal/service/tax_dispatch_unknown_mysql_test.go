package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	taxoutbox "github.com/j-s-te/settlement/internal/outbox"
)

// TestMySQLTaxDispatchUnknownRetainsReservation 验证发送后无可信回执不能释放税票额度。
// 参数：t为测试上下文，只使用显式SETTLEMENT_TEST_DSN独占测试库。
// 返回值：无；未知结果未持久化、无法对账或允许作废时失败。
func TestMySQLTaxDispatchUnknownRetainsReservation(t *testing.T) {
	db := refundFinanceMySQL(t)
	for _, response := range []string{"gateway-error", "incomplete-receipt", "legacy-unproven"} {
		t.Run(response, func(t *testing.T) {
			ctx := t.Context()
			actor := roleFlowPrincipal("settlement_accountant", "applicant", newID())
			reviewer := roleFlowPrincipal("settlement_reviewer", "reviewer", actor.TenantID)
			receivable := seedDifferenceReceivable(t, db, actor.TenantID)
			var snapshot string
			if err := db.QueryRow(`SELECT contract_snapshot_id FROM settlement_receivable WHERE id=?`, receivable).Scan(&snapshot); err != nil {
				t.Fatal(err)
			}
			svc := &Service{DB: db, InvoiceIssuanceMode: "tax_adapter"}
			request, err := svc.CreateInvoiceRequest(ctx, actor, newID(), InvoiceRequestInput{
				ContractSnapshotID: snapshot, InvoiceType: "VAT_NORMAL",
				BuyerProfile: map[string]any{"name": "隔离税控客户"},
				Items: []InvoiceItemInput{{ItemName: "服务", Quantity: "1", UnitPriceExclTax: "100",
					AmountExclTax: "100", TaxRate: "0", TaxAmount: "0", AmountInclTax: "100"}},
				Allocations: []InvoiceAllocationInput{{ReceivableID: receivable, ReservedAmount: "100"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := svc.ApproveInvoiceRequest(ctx, reviewer, request, 1)
			if err != nil || attempt == "" {
				t.Fatalf("真实审批失败: attempt=%s err=%v", attempt, err)
			}
			if _, err = db.Exec(`UPDATE settlement_outbox_event SET attempt_count=6
			 WHERE tenant_id=? AND destination='TAX_INVOICE_COMMAND'`, actor.TenantID); err != nil {
				t.Fatal(err)
			}
			deliveryCtx, stopDelivery := context.WithCancel(ctx)
			defer stopDelivery()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					fmt.Fprint(w, `{"access_token":"synthetic-token","token_type":"Bearer","expires_in":300,"scope":"tax.write"}`)
					return
				}
				calls.Add(1)
				stopDelivery()
				if response == "gateway-error" {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				fmt.Fprint(w, `{"code":"OK","data":{"receipts":[]}}`)
			}))
			defer server.Close()
			worker := &taxoutbox.Worker{Store: &taxoutbox.Store{DB: db, WorkerID: newID(), Lease: time.Minute},
				HTTP: server.Client(), BatchSize: 1, Destinations: []taxoutbox.Destination{{
					Name: "TAX_INVOICE_COMMAND", Endpoint: server.URL + "/tax",
					TokenEndpoint: server.URL + "/token", Scope: "tax.write",
				}}}
			if response == "legacy-unproven" {
				_, err = db.Exec(`UPDATE settlement_outbox_event SET status='DEAD_LETTER'
                WHERE tenant_id=? AND destination='TAX_INVOICE_COMMAND'`, actor.TenantID)
				if err != nil {
					t.Fatal(err)
				}
			} else if err = worker.Run(deliveryCtx); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			var status, command string
			var reconcilable, neverAccepted bool
			if err = db.QueryRow(`SELECT status,next_reconcile_at IS NOT NULL,accepted_at IS NULL,
			 command_event_id FROM settlement_invoice_issue_attempt WHERE id=?`, attempt).
				Scan(&status, &reconcilable, &neverAccepted, &command); err != nil {
				t.Fatal(err)
			}
			wantCalls, wantStatus, wantReconcile := int32(1), "UNKNOWN", true
			if response == "legacy-unproven" {
				wantCalls, wantStatus, wantReconcile = 0, "PENDING", false
			}
			if calls.Load() != wantCalls || status != wantStatus || reconcilable != wantReconcile || !neverAccepted {
				t.Errorf("发送后未知结果未保持: calls=%d status=%s reconcile=%t noAck=%t",
					calls.Load(), status, reconcilable, neverAccepted)
			}
			var version int
			if err = db.QueryRow(`SELECT version FROM settlement_invoice_request WHERE id=?`, request).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err = svc.CancelInvoiceRequest(ctx, reviewer, newID(), request, version, "税控结果未知时不可释放"); !errors.Is(err, ErrConflict) {
				t.Errorf("未知结果允许作废: %v", err)
			}
			var allocationState, commandState string
			if err = db.QueryRow(`SELECT status FROM settlement_invoice_request_allocation
			 WHERE invoice_request_id=?`, request).Scan(&allocationState); err != nil {
				t.Fatal(err)
			}
			if err = db.QueryRow(`SELECT status FROM settlement_outbox_event WHERE event_id=?`, command).Scan(&commandState); err != nil {
				t.Fatal(err)
			}
			if allocationState != "RESERVED" || commandState != "DEAD_LETTER" {
				t.Errorf("额度/死信状态=%s/%s", allocationState, commandState)
			}
		})
	}
}
