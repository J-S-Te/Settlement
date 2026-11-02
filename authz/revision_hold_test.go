package authz

import (
	"encoding/json"
	"testing"
)

// TestRevisionHoldPermissionAndRoleMapping 用途：核对合同修订人工复核工单已声明
// 读取与关单能力，且关单只授予财务负责人，不扩散到销售、助理或只读角色。
// 参数：t 为测试上下文。
// 返回值：无；权限缺失、风险等级不符或角色越权授权时失败。
func TestRevisionHoldPermissionAndRoleMapping(t *testing.T) {
	var manifest struct {
		Permissions []struct {
			Code      string `json:"code"`
			Action    string `json:"action"`
			Resource  string `json:"resource_code"`
			RiskLevel string `json:"risk_level"`
		} `json:"permissions"`
		Roles []struct {
			Code        string   `json:"code"`
			Permissions []string `json:"permissions"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(PermissionManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	type expectation struct {
		code, action, resource, risk string
	}
	wants := []expectation{
		{"settlement.revision.read", "read", "contract_revision", "LOW"},
		{"settlement.revision.resolve", "resolve", "contract_revision", "HIGH"},
	}
	declared := map[string]bool{}
	for _, permission := range manifest.Permissions {
		for _, want := range wants {
			if permission.Code != want.code {
				continue
			}
			declared[want.code] = true
			if permission.Action != want.action || permission.Resource != want.resource || permission.RiskLevel != want.risk {
				t.Fatalf("%s 权限元数据不符: %+v", want.code, permission)
			}
		}
	}
	for _, want := range wants {
		if !declared[want.code] {
			t.Fatalf("%s 未在目录声明", want.code)
		}
	}
	roles := map[string]map[string]bool{}
	for _, role := range manifest.Roles {
		roles[role.Code] = map[string]bool{}
		for _, granted := range role.Permissions {
			roles[role.Code][granted] = true
		}
	}
	if !roles["settlement_finance_manager"]["settlement.revision.read"] || !roles["settlement_finance_manager"]["settlement.revision.resolve"] {
		t.Fatal("财务经理缺少合同修订工单的读取或关单能力")
	}
	if !roles["settlement_accountant"]["settlement.revision.read"] {
		t.Fatal("结算会计缺少合同修订工单的只读能力")
	}
	for _, code := range []string{"settlement_sales", "settlement_project_assistant", "settlement_management", "settlement_system_admin", "settlement_sales_director", "settlement_reviewer"} {
		if roles[code]["settlement.revision.resolve"] {
			t.Fatalf("%s 不得关闭合同修订工单", code)
		}
	}
}
