package sessiongovernance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCentralClientRejectsAmbiguousResponse 拒绝截断、超长或多个JSON响应，避免接受伪成功前缀。
// 参数：t为测试上下文。
// 返回值：无；无效协议正文未被识别为依赖错误时失败。
func TestCentralClientRejectsAmbiguousResponse(t *testing.T) {
	for _, body := range []string{`{} {"error":"invalid_session"}`, `{} ` + strings.Repeat(" ", 65<<10), `{"broken":`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		client, err := NewHTTPClient(server.URL, server.URL, "client", server.URL+"/callback")
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.State(context.Background(), "test-binding")
		server.Close()
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("ambiguous response accepted: %v", err)
		}
	}
}

// TestCentralClientSeparatesDependencyAndExpiry 验证临时故障不能冒充确定失效。
// 参数：t为测试上下文。
// 返回值：无；只有401确定失效、409绑定冲突，其余错误保持暂不可用。
func TestCentralClientSeparatesDependencyAndExpiry(t *testing.T) {
	for _, test := range []struct {
		code int
		want error
	}{{401, ErrInactive}, {409, ErrBindingConflict}, {503, ErrUnavailable}, {429, ErrUnavailable}, {302, ErrUnavailable}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://attacker.invalid")
			w.WriteHeader(test.code)
		}))
		client, err := NewHTTPClient(server.URL, server.URL, "client", server.URL+"/callback")
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.State(context.Background(), "test-binding")
		server.Close()
		if !errors.Is(err, test.want) {
			t.Fatalf("status%d got%v want%v", test.code, err, test.want)
		}
	}
}

// countingServer 统计真实到达平台的HTTP次数，用于证明缓存命中和失效行为。
type countingServer struct {
	*httptest.Server
	hits int32
}

// newCountingServer 用途：启动计数服务并注册测试结束清理。
// 参数：t为测试上下文，handler为真实响应逻辑。
// 返回值：带命中计数的服务。
func newCountingServer(t *testing.T, handler http.HandlerFunc) *countingServer {
	t.Helper()
	server := &countingServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&server.hits, 1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// count 用途：读取当前HTTP命中次数。
// 参数：无。
// 返回值：累计命中次数。
func (s *countingServer) count() int32 { return atomic.LoadInt32(&s.hits) }

// sessionClient 用途：构造指向计数服务的客户端。
// 参数：t为测试上下文，server为计数服务。
// 返回值：中央协议客户端。
func sessionClient(t *testing.T, server *countingServer) *HTTPClient {
	t.Helper()
	client, err := NewHTTPClient(server.URL, server.URL, "client", server.URL+"/callback")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// writeSessionContext 用途：返回带指定会话标识的合法JSON窗口。
// 参数：w为响应写入器，sessionID为会话标识。
// 返回值：无。
func writeSessionContext(w http.ResponseWriter, sessionID string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"session_id":"` + sessionID + `","tenant_id":"tenant"}`))
}

// TestStateCacheSameBindingSingleRequest 同一binding在TTL内只发一次只读查询。
// 参数：t为测试上下文。
// 返回值：无；重复请求或返回不同窗口时失败。
func TestStateCacheSameBindingSingleRequest(t *testing.T) {
	t.Setenv("CENTRAL_SESSION_CACHE_MS", "2000")
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"session-1","tenant_id":"tenant"}`))
	})
	client := sessionClient(t, server)

	first, err := client.State(context.Background(), "binding-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.State(context.Background(), "binding-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := server.count(); got != 1 {
		t.Fatalf("state cache miss: hits=%d want 1", got)
	}
	if first.SessionID != "session-1" || second.SessionID != "session-1" {
		t.Fatalf("unexpected contexts: %q %q", first.SessionID, second.SessionID)
	}
}

// TestStateCacheDifferentBindingsEachRequest 不同binding不共享缓存条目。
// 参数：t为测试上下文。
// 返回值：无；任一binding被另一条目命中时失败。
func TestStateCacheDifferentBindingsEachRequest(t *testing.T) {
	t.Setenv("CENTRAL_SESSION_CACHE_MS", "2000")
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSessionContext(w, "session-"+strings.TrimPrefix(r.Header.Get("Authorization"), "SessionBinding "))
	})
	client := sessionClient(t, server)

	first, err := client.State(context.Background(), "binding-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.State(context.Background(), "binding-b")
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID != "session-binding-a" || second.SessionID != "session-binding-b" {
		t.Fatalf("bindings crossed: %q %q", first.SessionID, second.SessionID)
	}
	if got := server.count(); got != 2 {
		t.Fatalf("distinct bindings shared cache: hits=%d want 2", got)
	}
}

// TestActivityNeverUsesStateCache 交互代理必须真实到达平台，不读也不写只读缓存。
// 参数：t为测试上下文。
// 返回值：无；Activity被缓存或State条目替代交互时失败。
func TestActivityNeverUsesStateCache(t *testing.T) {
	t.Setenv("CENTRAL_SESSION_CACHE_MS", "2000")
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/session/activity" {
			writeSessionContext(w, "activity")
			return
		}
		writeSessionContext(w, "state")
	})
	client := sessionClient(t, server)

	if _, err := client.State(context.Background(), "binding-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.State(context.Background(), "binding-a"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		value, err := client.Activity(context.Background(), "binding-a")
		if err != nil {
			t.Fatal(err)
		}
		if value.SessionID != "activity" {
			t.Fatalf("activity served from cache: %q", value.SessionID)
		}
	}
	if got := server.count(); got != 3 {
		t.Fatalf("activity not proxied or state not cached: hits=%d want 3", got)
	}
}

// TestStateCacheExpires 越过TTL后必须重新查询平台。
// 参数：t为测试上下文。
// 返回值：无；过期条目仍被复用时失败。
func TestStateCacheExpires(t *testing.T) {
	t.Setenv("CENTRAL_SESSION_CACHE_MS", "10")
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSessionContext(w, "session-1")
	})
	client := sessionClient(t, server)

	if _, err := client.State(context.Background(), "binding-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.State(context.Background(), "binding-a"); err != nil {
		t.Fatal(err)
	}
	if got := server.count(); got != 1 {
		t.Fatalf("cache miss within ttl: hits=%d want 1", got)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := client.State(context.Background(), "binding-a"); err != nil {
		t.Fatal(err)
	}
	if got := server.count(); got != 2 {
		t.Fatalf("expired entry reused: hits=%d want 2", got)
	}
}

// TestStateCacheInvalidatedByInactive 确定失效后删除缓存并立即报错，不回退旧窗口。
// 参数：t为测试上下文。
// 返回值：无；失效后仍返回缓存窗口或不再请求平台时失败。
func TestStateCacheInvalidatedByInactive(t *testing.T) {
	t.Setenv("CENTRAL_SESSION_CACHE_MS", "10")
	var revoked int32
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&revoked) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeSessionContext(w, "session-1")
	})
	client := sessionClient(t, server)

	if _, err := client.State(context.Background(), "binding-a"); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt32(&revoked, 1)
	time.Sleep(30 * time.Millisecond)
	if _, err := client.State(context.Background(), "binding-a"); !errors.Is(err, ErrInactive) {
		t.Fatalf("revoked session not reported: %v", err)
	}
	if _, err := client.State(context.Background(), "binding-a"); !errors.Is(err, ErrInactive) {
		t.Fatalf("inactive result was cached: %v", err)
	}
	if got := server.count(); got != 3 {
		t.Fatalf("inactive did not drop cache entry: hits=%d want 3", got)
	}
}

// TestStateCacheDisabledByZero CENTRAL_SESSION_CACHE_MS=0 完全关闭只读缓存。
// 参数：t为测试上下文。
// 返回值：无；关闭后仍有缓存命中时失败。
func TestStateCacheDisabledByZero(t *testing.T) {
	t.Setenv("CENTRAL_SESSION_CACHE_MS", "0")
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSessionContext(w, "session-1")
	})
	client := sessionClient(t, server)

	for i := 0; i < 2; i++ {
		if _, err := client.State(context.Background(), "binding-a"); err != nil {
			t.Fatal(err)
		}
	}
	if got := server.count(); got != 2 {
		t.Fatalf("cache active while disabled: hits=%d want 2", got)
	}
}

// TestStateCacheInvalidValueFallsBackToDefault 非法配置回落默认TTL而不是关闭或无限缓存。
// 参数：t为测试上下文。
// 返回值：无；TTL不为默认2000ms时失败。
func TestStateCacheInvalidValueFallsBackToDefault(t *testing.T) {
	t.Setenv("CENTRAL_SESSION_CACHE_MS", "not-a-number")
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSessionContext(w, "session-1")
	})
	client := sessionClient(t, server)

	for i := 0; i < 2; i++ {
		if _, err := client.State(context.Background(), "binding-a"); err != nil {
			t.Fatal(err)
		}
	}
	if got := server.count(); got != 1 {
		t.Fatalf("default cache not active: hits=%d want 1", got)
	}
}

// TestStateCacheNeverServesStaleOnFailure 平台故障不写缓存、不返回旧值，保持fail-closed。
// 参数：t为测试上下文。
// 返回值：无；故障被缓存或旧窗口被返回时失败。
func TestStateCacheNeverServesStaleOnFailure(t *testing.T) {
	t.Setenv("CENTRAL_SESSION_CACHE_MS", "10")
	var failing int32
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&failing) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeSessionContext(w, "session-1")
	})
	client := sessionClient(t, server)

	if _, err := client.State(context.Background(), "binding-a"); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt32(&failing, 1)
	time.Sleep(30 * time.Millisecond)
	value, err := client.State(context.Background(), "binding-a")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("dependency failure hidden: %v", err)
	}
	if value.SessionID != "" {
		t.Fatalf("stale context returned on failure: %q", value.SessionID)
	}
	if got := server.count(); got != 2 {
		t.Fatalf("failure was cached: hits=%d want 2", got)
	}
}

// TestStateTimeoutIsBounded 只读查询按800ms独立收敛，慢平台不拖住请求生命周期。
// 参数：t为测试上下文。
// 返回值：无；超时未生效或耗时越过独立期限时失败。
func TestStateTimeoutIsBounded(t *testing.T) {
	t.Setenv("CENTRAL_SESSION_CACHE_MS", "0")
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1200 * time.Millisecond)
		writeSessionContext(w, "session-1")
	})
	client := sessionClient(t, server)

	started := time.Now()
	_, err := client.State(context.Background(), "binding-a")
	elapsed := time.Since(started)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("slow state got %v want %v", err, ErrUnavailable)
	}
	if elapsed < 700*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("state timeout not isolated: elapsed=%v", elapsed)
	}
}

// TestStateCacheConcurrent 并发只读查询不产生数据竞争且都得到合法窗口。
// 参数：t为测试上下文。
// 返回值：无；出现竞态、错误或空窗口时失败。
func TestStateCacheConcurrent(t *testing.T) {
	t.Setenv("CENTRAL_SESSION_CACHE_MS", "2000")
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeSessionContext(w, "session-1")
	})
	client := sessionClient(t, server)

	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 5; j++ {
				value, err := client.State(context.Background(), "binding-a")
				if err != nil || value.SessionID != "session-1" {
					t.Errorf("concurrent state got %q %v", value.SessionID, err)
					return
				}
			}
		}()
	}
	wait.Wait()
	if got := server.count(); got < 1 || got > 80 {
		t.Fatalf("unexpected concurrent hits: %d", got)
	}
}
