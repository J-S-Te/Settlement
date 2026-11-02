package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/settlement/internal/creditclient"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestRetryCreditSyncAuthorizationFailuresAreClassified 验证 CRM 授权/用途错误与
// 5xx 分开上报（BIZ-XAPI-02）：401/403 与平台令牌 invalid_scope 必须命中
// ErrCreditSyncAuthorization，5xx 保持普通投递失败；两者都继续记录 RETRY_WAIT，
// 修复接入配置后运营仍可显式重试。
// 参数：t为测试上下文，使用真实 creditclient 与脚本化数据库边界。
// 返回值：无；分类错误或数据库记录行为改变时测试失败。
func TestRetryCreditSyncAuthorizationFailuresAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		tokenReject  bool
		wantSentinel bool
	}{
		{name: "401 unauthorized", status: http.StatusUnauthorized, wantSentinel: true},
		{name: "403 forbidden", status: http.StatusForbidden, wantSentinel: true},
		{name: "token invalid_scope", status: http.StatusBadRequest, tokenReject: true, wantSentinel: true},
		{name: "500 server failure", status: http.StatusInternalServerError},
		{name: "503 unavailable", status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					w.Header().Set("Content-Type", "application/json")
					if tc.tokenReject {
						// 平台没有该用途时令牌端点返回 invalid_scope；这是配置漂移而非瞬时故障。
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprint(w, `{"error":"invalid_scope","error_description":"requested scope is not assigned to this client"}`)
						return
					}
					fmt.Fprint(w, `{"access_token":"credit-test-token","token_type":"Bearer","expires_in":300}`)
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			publisher, err := creditclient.New(server.URL+"/api/v1/internal/credit/payment-events", server.URL+"/token", "client", "secret", "customer.credit.payment.ingest", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			// 脚本要求：认领 -> PROCESSING -> commit -> 失败记录 RETRY_WAIT（不得写 DELIVERED）。
			db := scriptdb.Open(t,
				scriptdb.Step{Kind: "begin"},
				scriptdb.Step{Kind: "query", SQL: "status IN ('PENDING','RETRY_WAIT')", Rows: [][]driver.Value{{"row-1", []byte(`{}`)}}},
				scriptdb.Step{Kind: "exec", SQL: "status='PROCESSING'"},
				scriptdb.Step{Kind: "commit"},
				scriptdb.Step{Kind: "exec", SQL: "status='RETRY_WAIT',attempt_count=attempt_count+1"},
			)
			s := &Service{DB: db, CreditPublisher: publisher}
			err = s.RetryCreditSync(context.Background(), "tenant", "event")
			if err == nil {
				t.Fatal("expected delivery failure")
			}
			if got := errors.Is(err, ErrCreditSyncAuthorization); got != tc.wantSentinel {
				t.Fatalf("authorization classification = %v, want %v (error: %v)", got, tc.wantSentinel, err)
			}
		})
	}
}
