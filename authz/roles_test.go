package authz

import (
	"encoding/json"
	"testing"
)

// TestBusinessRoles 用途：核对业务角色存在及财务操作隔离。
// 参数：t 为测试上下文。
// 返回值：无，角色缺失或越权授权时测试失败。
func TestBusinessRoles(t *testing.T) {
	var m struct {
		Roles []struct {
			Code        string
			Permissions []string
		}
	}
	if err := json.Unmarshal(PermissionManifest, &m); err != nil {
		t.Fatal(err)
	}
	roles := map[string]map[string]bool{}
	for _, role := range m.Roles {
		roles[role.Code] = map[string]bool{}
		for _, grant := range role.Permissions {
			roles[role.Code][grant] = true
		}
	}
	for _, code := range []string{"settlement_sales", "settlement_project_assistant", "settlement_finance_manager", "settlement_sales_director", "settlement_management", "settlement_system_admin"} {
		if roles[code] == nil {
			t.Errorf("missing role %s", code)
		}
	}
	if !roles["settlement_accountant"]["settlement.invoice.issue"] {
		t.Error("会计缺少发票执行权限")
	}
	for _, code := range []string{"settlement_sales", "settlement_project_assistant", "settlement_management", "settlement_system_admin"} {
		for _, forbidden := range []string{"settlement.receipt.record", "settlement.allocation.confirm", "settlement.invoice.approve", "settlement.invoice.issue"} {
			if roles[code][forbidden] {
				t.Errorf("%s granted %s", code, forbidden)
			}
		}
	}
}
