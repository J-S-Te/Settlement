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

// TestPlanCoverageRouteRequiresAuthentication 验证计划差额查询不接受匿名请求。
// 参数：t为测试上下文。
// 返回值：无；匿名请求未返回401时失败。
func TestPlanCoverageRouteRequiresAuthentication(t *testing.T) {
	handler := New(nil, config.Config{PublicOrigin: "http://localhost:8081"}, slog.Default(), &platform.Authenticator{}, nil, nil)
	for _, path := range []string{"/api/v1/plan-coverage", "/api/v1/plan-coverage?contract_snapshot_id=snapshot-1"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Origin", "http://localhost:8081")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
}

// TestPlanCoverageListReturnsGaps 验证差额列表按租户绑定并返回差额与阈值标记。
// 参数：t为测试上下文。
// 返回值：无；查询参数或响应结构不符时失败。
func TestPlanCoverageListReturnsGaps(t *testing.T) {
	var args []driver.NamedValue
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_snapshot cs LEFT JOIN settlement_receivable_plan rp", Args: &args, Rows: [][]driver.Value{
		{"snapshot-1", "C-1", "100.00", "94.50", int64(1)},
	}})
	handler := New(db, config.Config{DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/plan-coverage", nil))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `"gap":"5.50"`) || !strings.Contains(body, `"exceeds_threshold":true`) || !strings.Contains(body, `"contract_event_plan":true`) {
		t.Fatalf("status=%d body=%s", w.Code, body)
	}
	if len(args) == 0 || args[0].Value != "dev" {
		t.Fatalf("租户绑定参数=%+v", args)
	}
}

// TestPlanCoverageSingleContractReturnsGap 验证单合同差额查询走同一覆盖口径。
// 参数：t为测试上下文。
// 返回值：无；响应结构不符时失败。
func TestPlanCoverageSingleContractReturnsGap(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_snapshot cs LEFT JOIN settlement_receivable_plan rp", Rows: [][]driver.Value{
		{"C-1", "100.00", "80.00", int64(0)},
	}})
	handler := New(db, config.Config{DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/plan-coverage?contract_snapshot_id=snapshot-1", nil))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `"gap":"20.00"`) || !strings.Contains(body, `"contract_snapshot_id":"snapshot-1"`) {
		t.Fatalf("status=%d body=%s", w.Code, body)
	}
}
