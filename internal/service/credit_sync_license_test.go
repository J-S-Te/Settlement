package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/j-s-te/settlement/internal/creditclient"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestRetryCreditSyncTokenExpiryKeepsClaimRecoverable 验证RetryCreditSyncTokenExpiryKeepsClaimRecoverable的业务约束与回归行为。
// 参数：t为测试上下文，使用独立夹具。
// 返回值：无；预期结果或状态不一致时测试失败。
func TestRetryCreditSyncTokenExpiryKeepsClaimRecoverable(t *testing.T) {
	var expired atomic.Bool
	var tokens, posts atomic.Int32
	denied := errors.New("license expired during synchronous OAuth request")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens.Add(1)
			expired.Store(true)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"sync-credit-test-token","token_type":"Bearer","expires_in":300}`)
			return
		}
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	publisher, err := creditclient.New(server.URL+"/events", server.URL+"/token", "client", "secret", "credit.write", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	publisher.SetBusinessLicenseCheck(func(context.Context) error {
		if expired.Load() {
			return denied
		}
		return nil
	})
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "status IN ('PENDING','RETRY_WAIT')", Rows: [][]driver.Value{{"row-1", []byte(`{}`)}}},
		scriptdb.Step{Kind: "exec", SQL: "status='PROCESSING',locked_at=UTC_TIMESTAMP(3)"},
		scriptdb.Step{Kind: "commit"},
	)
	s := &Service{DB: db, CreditPublisher: publisher}
	if err := s.RetryCreditSync(context.Background(), "tenant", "event"); !errors.Is(err, denied) || tokens.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("synchronous license pause lost: token=%d posts=%d err=%v", tokens.Load(), posts.Load(), err)
	}
	// 不允许 RETRY_WAIT、增加尝试次数或 DELIVERED 更新；原认领的
	// locked_at 仍可供正常过期租约恢复。
}

// TestRetryCreditSyncNormalOutcomesStillRecorded 验证RetryCreditSyncNormalOutcomesStillRecorded的业务约束与回归行为。
// 参数：t为测试上下文，使用独立夹具。
// 返回值：无；预期结果或状态不一致时测试失败。
func TestRetryCreditSyncNormalOutcomesStillRecorded(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%v", failed), func(t *testing.T) {
			publishErr := errors.New("CRM unavailable")
			outcome := "status='DELIVERED'"
			if failed {
				outcome = "status='RETRY_WAIT',attempt_count=attempt_count+1"
			}
			db := scriptdb.Open(t,
				scriptdb.Step{Kind: "begin"},
				scriptdb.Step{Kind: "query", SQL: "status IN ('PENDING','RETRY_WAIT')", Rows: [][]driver.Value{{"row-1", []byte(`{}`)}}},
				scriptdb.Step{Kind: "exec", SQL: "status='PROCESSING'"},
				scriptdb.Step{Kind: "commit"},
				scriptdb.Step{Kind: "exec", SQL: outcome},
			)
			s := &Service{DB: db, CreditPublisher: licenseCreditPublisher{send: func(string) error {
				if failed {
					return publishErr
				}
				return nil
			}}}
			err := s.RetryCreditSync(context.Background(), "tenant", "event")
			if failed && !errors.Is(err, publishErr) || !failed && err != nil {
				t.Fatalf("normal outcome changed: %v", err)
			}
		})
	}
}
