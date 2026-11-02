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

// TestTaxCommandDeadLetterRoutesRequireAuthentication 验证死信列表和重试都不接受匿名请求。
// 参数：t 测试上下文。
// 返回值：无；匿名请求未返回401时失败。
func TestTaxCommandDeadLetterRoutesRequireAuthentication(t *testing.T) {
	handler := New(nil, config.Config{PublicOrigin: "http://localhost:8081"}, slog.Default(), &platform.Authenticator{}, nil, nil)
	for _, request := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/tax-command-dead-letters", ""},
		{http.MethodPost, "/api/v1/tax-command-dead-letters/outbox-1/retry", `{"reason":"核实税控命令死信"}`},
	} {
		r := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
		r.Header.Set("Origin", "http://localhost:8081")
		r.Header.Set("X-CSRF-Token", "1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d body=%s", request.method, request.path, w.Code, w.Body.String())
		}
	}
}

// TestTaxCommandDeadLetterRetryRejectsCSRF 验证财务重试仍受CSRF保护。
// 参数：t 测试上下文。
// 返回值：无；缺少CSRF令牌未被拒绝时失败。
func TestTaxCommandDeadLetterRetryRejectsCSRF(t *testing.T) {
	handler := New(nil, config.Config{PublicOrigin: "http://localhost:8081", DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tax-command-dead-letters/outbox-1/retry", strings.NewReader(`{"reason":"核实税控命令死信"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestTaxCommandDeadLettersListUsesTenantBoundQuery 验证HTTP死信视图把租户和过滤条件
// 交给数据库，并原样返回死信事实。
// 参数：t 测试上下文。
// 返回值：无；未查询数据库或响应结构不符时失败。
func TestTaxCommandDeadLettersListUsesTenantBoundQuery(t *testing.T) {
	var args []driver.NamedValue
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "settlement_outbox_event", Args: &args, Rows: [][]driver.Value{
		{"outbox-1", "event-1", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "request-1", int64(7), "DELIVERY_FAILED", "终态失败", "2026-01-02T03:04:05.000000Z", "2026-01-01T03:04:05.000000Z", "attempt-1", "request-1", "PENDING", "", "", "", "ISSUE_PENDING", ""},
	}})
	handler := New(db, config.Config{DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/tax-command-dead-letters?invoice_request_id=request-1", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"operation_type":"INVOICE_ISSUE"`) || !strings.Contains(w.Body.String(), `"last_error_code":"DELIVERY_FAILED"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(args) != 3 || args[0].Value != "dev" || args[1].Value != "request-1" || args[2].Value != "request-1" {
		t.Fatalf("tenant scoped query args=%+v", args)
	}
}

// TestTaxCommandDeadLetterRetryRequiresIdempotencyKey 验证HTTP重试缺少幂等键时
// 在进入业务事务前失败。
// 参数：t 测试上下文。
// 返回值：无；缺键请求未返回400时失败。
func TestTaxCommandDeadLetterRetryRequiresIdempotencyKey(t *testing.T) {
	handler := New(nil, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tax-command-dead-letters/outbox-1/retry", strings.NewReader(`{"reason":"核实税控命令死信"}`))
	r.Header.Set("Origin", "https://settlement.test")
	r.Header.Set("X-CSRF-Token", "1")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestTaxCommandDeadLetterRetryResetsCommandThroughHTTP 验证财务重试通过HTTP
// 只重置同一条死信命令并返回待投递状态。
// 参数：t 测试上下文。
// 返回值：无；事务边界或响应状态不符时失败。
func TestTaxCommandDeadLetterRetryResetsCommandThroughHTTP(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_outbox_event", Rows: [][]driver.Value{{"TAX_INVOICE_COMMAND", "DEAD_LETTER", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "event-1", "invoice_request", []byte(`{"attempt_id":"attempt-1"}`), "FAILED", "终态失败"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_issue_attempt", Rows: [][]driver.Value{{"PENDING", true, "request-1", "event-1", int64(1), "ISSUE_PENDING"}}},
		scriptdb.Step{Kind: "query", SQL: "newer.attempt_no>?", Rows: [][]driver.Value{{true}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_snapshot_id FROM settlement_invoice_request", Rows: [][]driver.Value{{"snapshot-1"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "exec", SQL: "SET status='PENDING',attempt_count=0"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "commit"},
	)
	handler := New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tax-command-dead-letters/outbox-1/retry", strings.NewReader(`{"reason":"核实税控命令死信"}`))
	r.Header.Set("Origin", "https://settlement.test")
	r.Header.Set("X-CSRF-Token", "1")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "retry-key-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"outbox-1"`) || !strings.Contains(w.Body.String(), `"status":"PENDING"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
