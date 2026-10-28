package projectreference

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestResolveValidatesTenantAndReference 验证机器调用与权威项目引用绑定。
// 参数：t 为测试上下文。
// 返回值：无。
func TestResolveValidatesTenantAndReference(t *testing.T) {
	tenant := "tenant"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_ = r.ParseForm()
			if r.Form.Get("scope") != "settlement.project_reference.read" {
				t.Error("机器scope错误")
			}
			fmt.Fprint(w, `{"access_token":"machine","token_type":"Bearer","expires_in":60}`)
			return
		}
		if r.URL.Path != "/internal/v1/settlement/project-references/P1" || r.Header.Get("Authorization") != "Bearer machine" {
			t.Error("权威端点或令牌缺失")
		}
		fmt.Fprintf(w, `{"code":"OK","data":{"tenant_id":%q,"project_id":"P1","contract_id":"C1","customer_id":"42","name":"项目","version":3,"active":true}}`, tenant)
	}))
	defer server.Close()
	c, err := New(server.URL, "client", "secret", server.URL+"/token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := c.Resolve(context.Background(), "tenant", "P1")
	if err != nil || ref.ContractID != "C1" || ref.Version != 3 {
		t.Fatalf("引用=%+v,%v", ref, err)
	}
	tenant = "other"
	if _, err = c.Resolve(context.Background(), "tenant", "P1"); err == nil {
		t.Fatal("跨租户响应未拒绝")
	}
}
