package projectreference

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestResolveSurfacesInactiveAuthority 验证机器接口不会把权威项目失效当作解析失败：
// 调用方需要拿到 active=false 才能把本地快照从 true 收口为 false，
// 否则结算侧会出现 active 只升不降的陈旧范围。
// 参数：t 测试上下文。
// 返回值：无。
func TestResolveSurfacesInactiveAuthority(t *testing.T) {
	tenant := "tenant"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"machine","token_type":"Bearer","expires_in":60}`)
			return
		}
		fmt.Fprintf(w, `{"code":"OK","data":{"tenant_id":%q,"project_id":"P1","contract_id":"C1","customer_id":"42","name":"项目","version":2,"active":false}}`, tenant)
	}))
	defer server.Close()
	client, err := New(server.URL, "client", "secret", server.URL+"/token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := client.Resolve(context.Background(), tenant, "P1")
	if err != nil {
		t.Fatalf("inactive authority must be surfaced, not rejected: %v", err)
	}
	if ref.Active || ref.Version != 2 || ref.ContractID != "C1" {
		t.Fatalf("reference=%+v", ref)
	}
}
