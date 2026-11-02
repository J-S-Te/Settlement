package sessiongovernance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
