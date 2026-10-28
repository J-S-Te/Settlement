package httpapi

import (
	"database/sql"
	"database/sql/driver"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestRoleFinanceListsUseBoundedPages 验证财务列表把分页交给数据库并保持对象包络。
// 参数：t 测试上下文。
// 返回值：无；丢弃分页、缺少租户边界或改变响应结构时失败。
func TestRoleFinanceListsUseBoundedPages(t *testing.T) {
	for _, path := range []string{"receipt-differences", "accounting-vouchers"} {
		t.Run(path, func(t *testing.T) {
			connector := &pageConnector{t: t}
			db := sql.OpenDB(connector)
			defer db.Close()
			handler := New(db, config.Config{DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/"+path+"?limit=17&offset=102", nil))
			if response.Code != 200 || !connector.queried || strings.TrimSpace(response.Body.String()) != `{"data":{"items":[]}}` {
				t.Fatalf("HTTP=%d body=%s queried=%v", response.Code, response.Body.String(), connector.queried)
			}
		})
	}
}

// TestRoleFinanceListsRejectInvalidQuery 验证HTTP查询边界错误不会进入数据库。
// 参数：t 测试上下文。
// 返回值：无；越界、非法状态或长关键词未返回400时失败。
func TestRoleFinanceListsRejectInvalidQuery(t *testing.T) {
	for _, path := range []string{"receipt-differences", "accounting-vouchers"} {
		for _, query := range []string{"limit=0", "limit=101", "offset=-1", "offset=100001", "status=INVALID", "keyword=" + strings.Repeat("a", 129)} {
			handler := New(nil, config.Config{DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/"+path+"?"+query, nil))
			if response.Code != 400 {
				t.Fatalf("%s?%s HTTP=%d body=%s", path, query, response.Code, response.Body.String())
			}
		}
	}
}

// TestRoleRefundRouteAcceptsFactsWithoutRelaxingDecode 验证正式路由接纳回录DTO且继续拒绝未知字段。
// 参数：t 测试上下文。
// 返回值：无；有效事实不能提交或未知字段放行时失败。
func TestRoleRefundRouteAcceptsFactsWithoutRelaxingDecode(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		steps := []scriptdb.Step{}
		if !unknown {
			steps = []scriptdb.Step{{Kind: "begin"}, {Kind: "query", SQL: "FOR UPDATE", Rows: [][]driver.Value{{"REFUND_PENDING", "REFUND_PENDING", "accountant", "accountant", "manager", "receipt", int64(3)}}}, {Kind: "query", SQL: "DATE_FORMAT(receipt_date", Rows: [][]driver.Value{{"2020-01-01"}}}, {Kind: "exec", SQL: "status='REFUND_CONFIRMED'"}, {Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, {Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}, {Kind: "commit"}}
		}
		db := scriptdb.Open(t, steps...)
		handler := New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
		body := `{"version":3,"reference":"external-refund","completed_at":"2020-01-02T10:00:00+08:00","reason":"退款已完成"`
		if unknown {
			body += `,"unexpected":true`
		}
		request := httptest.NewRequest("POST", "/api/v1/receipt-differences/difference/confirm-refund", strings.NewReader(body+"}"))
		request.Header.Set("Origin", "https://settlement.test")
		request.Header.Set("X-CSRF-Token", "1")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := 200
		if unknown {
			want = 400
		}
		if response.Code != want {
			t.Fatalf("unknown=%v HTTP=%d body=%s", unknown, response.Code, response.Body.String())
		}
	}
}
