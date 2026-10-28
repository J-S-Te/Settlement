package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestReminderRolesUseRealDirectoryClient 用途：验证升级目录角色可穿过真实HTTP客户端白名单。
// 参数：t为测试上下文。
// 返回值：无，拒绝财务经理或管理层或没有请求平台时失败。
func TestReminderRolesUseRealDirectoryClient(t *testing.T) {
	queries := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth2/token" {
			w.Write([]byte(`{"access_token":"test-token","token_type":"Bearer","scope":"owner_directory.read"}`))
			return
		}
		queries++
		role := r.URL.Query().Get("role_code")
		if role != "settlement_finance_manager" && role != "settlement_management" {
			t.Errorf("错误升级角色 %s", role)
		}
		w.Write([]byte(`{"data":{"items":[{"user_id":"person-a","display_name":"隔离人员"}],"total":1}}`))
	}))
	defer server.Close()
	c := NewPersonnelDirectory(server.URL, "test-client", "test-secret", time.Second).(*HTTPPersonnelDirectory)
	for _, role := range []string{"settlement_finance_manager", "settlement_management"} {
		items, total, err := c.ListRole(context.Background(), role, "", 1, 100)
		if err != nil || total != 1 || len(items) != 1 {
			t.Fatalf("%s %v %d %v", role, items, total, err)
		}
	}
	if queries != 2 {
		t.Fatalf("未真正请求人员目录: %d", queries)
	}
}
