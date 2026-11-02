package httpapi

import (
	"database/sql/driver"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestInvoiceCancelRouteRequiresAuthentication 验证作废入口不接受匿名请求。
// 参数：t为测试上下文。
// 返回值：无；匿名请求未返回401时失败。
func TestInvoiceCancelRouteRequiresAuthentication(t *testing.T) {
	handler := New(nil, config.Config{PublicOrigin: "http://localhost:8081"}, slog.Default(), &platform.Authenticator{}, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-requests/request-1/cancel", strings.NewReader(`{"version":3,"reason":"税控命令死信后放弃开票"}`))
	r.Header.Set("Origin", "http://localhost:8081")
	r.Header.Set("X-CSRF-Token", "1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestInvoiceCancelRouteRejectsCSRF 验证作废仍受CSRF保护。
// 参数：t为测试上下文。
// 返回值：无；缺少CSRF令牌未被拒绝时失败。
func TestInvoiceCancelRouteRejectsCSRF(t *testing.T) {
	handler := New(nil, config.Config{PublicOrigin: "http://localhost:8081", DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-requests/request-1/cancel", strings.NewReader(`{"version":3,"reason":"税控命令死信后放弃开票"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestInvoiceCancelRouteRequiresIdempotencyKey 验证缺少幂等键时在进入业务事务前失败。
// 参数：t为测试上下文。
// 返回值：无；缺键请求未返回400时失败。
func TestInvoiceCancelRouteRequiresIdempotencyKey(t *testing.T) {
	handler := New(nil, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-requests/request-1/cancel", strings.NewReader(`{"version":3,"reason":"税控命令死信后放弃开票"}`))
	r.Header.Set("Origin", "https://settlement.test")
	r.Header.Set("X-CSRF-Token", "1")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestInvoiceCancelThroughHTTP 验证死信后作废通过HTTP把申请置为已作废并释放预留。
// 参数：t为测试上下文。
// 返回值：无；事务边界、锁定顺序或响应状态不符时失败。
func TestInvoiceCancelThroughHTTP(t *testing.T) {
	var auditArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id,COALESCE(command_event_id,'') FROM settlement_invoice_issue_attempt", Rows: [][]driver.Value{{"attempt-1", "event-1"}}},
		scriptdb.Step{Kind: "query", SQL: "settlement_outbox_event WHERE tenant_id=? AND event_id=? ORDER BY created_at DESC,id DESC LIMIT 1 FOR UPDATE", Rows: [][]driver.Value{{"TAX_INVOICE_COMMAND", "DEAD_LETTER"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_issue_attempt WHERE tenant_id=? AND id=? FOR UPDATE", Rows: [][]driver.Value{{"PENDING", true, "event-1"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_request WHERE id=? AND tenant_id=? FOR UPDATE", Rows: [][]driver.Value{{"ISSUE_PENDING", "applicant", int64(3)}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_snapshot_id FROM settlement_invoice_request", Rows: [][]driver.Value{{"snapshot-1"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT COUNT(*) FROM settlement_tax_invoice", Rows: [][]driver.Value{{int64(0)}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT COUNT(*) FROM settlement_tax_result_inbox", Rows: [][]driver.Value{{int64(0)}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_issue_attempt SET status='ABANDONED'"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_request SET status='CANCELLED'"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_request_allocation SET released_amount=reserved_amount-invoiced_amount"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: &auditArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "commit"},
	)
	handler := New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-requests/request-1/cancel", strings.NewReader(`{"version":3,"reason":"税控命令死信后放弃开票"}`))
	r.Header.Set("Origin", "https://settlement.test")
	r.Header.Set("X-CSRF-Token", "1")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "cancel-key-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"CANCELLED"`) || !strings.Contains(w.Body.String(), `"version":4`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(auditArgs) < 8 {
		t.Fatalf("缺少作废审计: %+v", auditArgs)
	}
	if eventType, _ := auditArgs[4].Value.(string); eventType != "SETTLEMENT_INVOICE_CANCELLED" {
		t.Fatalf("审计事件类型=%v", auditArgs[4].Value)
	}
}
