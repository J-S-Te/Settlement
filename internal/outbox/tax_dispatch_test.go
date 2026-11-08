package outbox

import (
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestTaxProtectionFailurePreservesBatch 验证保护事务失败直接返回且不记同行投递失败。
// 参数：t为测试上下文；使用SQL边界驱动模拟数据库失败或已过期的实际租约。
// 返回值：无；发生HTTP、重试计数SQL或吞掉保护错误时失败。
func TestTaxProtectionFailurePreservesBatch(t *testing.T) {
	storageErr := errors.New("synthetic storage failure")
	for _, tc := range []struct {
		name string
		live bool
		err  error
	}{
		{name: "expired-lease", err: ErrLeaseLost},
		{name: "storage-failure", live: true, err: storageErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := scriptdb.Step{Kind: "query", SQL: "SELECT destination,status"}
			if tc.err == storageErr {
				query.Err = storageErr
			} else {
				query.Rows = [][]driver.Value{{"TAX_INVOICE_COMMAND", "PROCESSING", "worker", "event-a", tc.live}}
			}
			db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, query, scriptdb.Step{Kind: "rollback"})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer server.Close()
			store := &Store{DB: db, WorkerID: "worker"}
			w := &Worker{Store: store, HTTP: server.Client(), tokens: map[string]cachedToken{
				"TAX_INVOICE_COMMAND": {Value: "synthetic", Expires: time.Now().Add(time.Hour)},
			}}
			destination := Destination{Name: "TAX_INVOICE_COMMAND", Endpoint: server.URL}
			events := []Event{
				{ID: "outbox-a", TenantID: "tenant", EventID: "event-a", Destination: destination.Name, Payload: []byte(`{"attempt_id":"a"}`)},
				{ID: "outbox-b", TenantID: "tenant", EventID: "event-b", Destination: destination.Name, Payload: []byte(`{"attempt_id":"b"}`)},
			}
			if err := w.deliverBatch(t.Context(), destination, events, store); !errors.Is(err, tc.err) {
				t.Fatalf("保护错误应直接返回: got=%v want=%v", err, tc.err)
			}
			if calls.Load() != 0 {
				t.Fatalf("保护失败仍触发HTTP: calls=%d", calls.Load())
			}
		})
	}
}
