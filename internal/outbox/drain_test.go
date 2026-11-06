package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeOutboxStore 记录排空语义关心的存储交互：领取批次、完成标记、失败标记。
// Delivered/Failed 与真实数据库驱动一致：上下文已取消时拒绝执行。
type fakeOutboxStore struct {
	events       []Event
	acquired     []string
	delivered    []Event
	failed       int
	cancelledErr []error
}

func (f *fakeOutboxStore) Acquire(_ context.Context, destination string, _ int) ([]Event, error) {
	f.acquired = append(f.acquired, destination)
	if len(f.acquired) > 1 {
		return nil, nil
	}
	return f.events, nil
}

func (f *fakeOutboxStore) filterImportNotifications(_ context.Context, events []Event) ([]Event, error) {
	return events, nil
}

func (f *fakeOutboxStore) Delivered(ctx context.Context, events []Event) error {
	if err := ctx.Err(); err != nil {
		f.cancelledErr = append(f.cancelledErr, err)
		return err
	}
	f.delivered = append(f.delivered, events...)
	return nil
}

func (f *fakeOutboxStore) Failed(ctx context.Context, _ []Event, _, _ string, _ bool) error {
	if err := ctx.Err(); err != nil {
		f.cancelledErr = append(f.cancelledErr, err)
		return err
	}
	f.failed++
	return nil
}

// drainTestWorker 装配一个只使用测试存储与本地 HTTP 服务的工作器。
func drainTestWorker(store eventStore, server *httptest.Server, destinations ...string) *Worker {
	destinationsPlan := make([]Destination, 0, len(destinations))
	for _, name := range destinations {
		destinationsPlan = append(destinationsPlan, Destination{
			Name: name, Endpoint: server.URL + "/deliver",
			TokenEndpoint: server.URL + "/token", Scope: "essential.write",
			ApplicationCode: "settlement", EnvironmentCode: "prod",
		})
	}
	return &Worker{testStore: store, HTTP: server.Client(), Destinations: destinationsPlan, tokens: map[string]cachedToken{}}
}

func drainTestServer(onDeliver func()) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"token","token_type":"Bearer","expires_in":300,"scope":"essential.write"}`)
			return
		}
		onDeliver()
		fmt.Fprint(w, `{"code":"OK","data":{"receipts":[{"event_id":"event-a","status":"ACCEPTED"}]}}`)
	}))
}

// TestRunOnceCompletesInFlightBatchAfterCancellation 验证批次中途取消：
// 当前批必须完成落库，且不再领取下一批（不破坏租约与可见性语义）。
func TestRunOnceCompletesInFlightBatchAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeOutboxStore{events: []Event{{ID: "1", EventID: "event-a", Destination: "PLATFORM_NOTIFICATION", EventType: "SETTLEMENT_TEST", Payload: json.RawMessage(`{}`)}}}
	server := drainTestServer(cancel) // 批次投递途中收到停机信号
	defer server.Close()
	w := drainTestWorker(store, server, "PLATFORM_NOTIFICATION", "PLATFORM_AUDIT")

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce returned error after drain: %v (cancelled store calls=%v)", err, store.cancelledErr)
	}
	if len(store.delivered) != 1 || store.delivered[0].EventID != "event-a" {
		t.Fatalf("in-flight batch was not completed: delivered=%v failed=%d cancelled=%v", store.delivered, store.failed, store.cancelledErr)
	}
	if len(store.acquired) != 1 || store.acquired[0] != "PLATFORM_NOTIFICATION" {
		t.Fatalf("outbox acquired batches beyond the in-flight one: %v", store.acquired)
	}
}

// TestRunOnceStopsAcquiringAfterCancellation 验证取消后不领取“新”批次。
func TestRunOnceStopsAcquiringAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 进入扫描前已停机
	store := &fakeOutboxStore{events: []Event{{ID: "1", EventID: "event-a", Destination: "PLATFORM_AUDIT", Payload: json.RawMessage(`{}`)}}}
	server := drainTestServer(func() {})
	defer server.Close()
	w := drainTestWorker(store, server, "PLATFORM_AUDIT", "PLATFORM_NOTIFICATION")

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce returned error after shutdown: %v", err)
	}
	if len(store.acquired) != 0 {
		t.Fatalf("outbox acquired batches after shutdown: %v", store.acquired)
	}
}

// TestRunOncePersistsDeliveryFailureAfterCancellation 验证失败分支同样在取消后落库。
func TestRunOncePersistsDeliveryFailureAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeOutboxStore{events: []Event{{ID: "1", EventID: "event-a", Destination: "PLATFORM_AUDIT", Payload: json.RawMessage(`{"actor_id":"user-1"}`)}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"token","token_type":"Bearer","expires_in":300,"scope":"essential.write"}`)
			return
		}
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	w := drainTestWorker(store, server, "PLATFORM_AUDIT")

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce returned error after drain: %v (cancelled store calls=%v)", err, store.cancelledErr)
	}
	if store.failed != 1 || len(store.delivered) != 0 {
		t.Fatalf("delivery failure was not persisted: failed=%d delivered=%v cancelled=%v", store.failed, store.delivered, store.cancelledErr)
	}
	if len(store.acquired) != 1 {
		t.Fatalf("outbox acquired batches beyond the in-flight one: %v", store.acquired)
	}
}
