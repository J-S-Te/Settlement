package authz

import (
	"encoding/json"
	"testing"
)

// TestFinanceOverridePermissionAndRoleMapping 用途：核对计划差额特批权限已声明，
// 且只映射到财务负责人角色，不扩散到销售或项目助理。
// 参数：t 为测试上下文。
// 返回值：无；权限缺失、风险等级不符或角色越权授权时测试失败。
func TestFinanceOverridePermissionAndRoleMapping(t *testing.T) {
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
	const override = "settlement.finance.override"
	declared := false
	for _, permission := range manifest.Permissions {
		if permission.Code != override {
			continue
		}
		declared = true
		if permission.RiskLevel != "HIGH" || permission.Action != "override" || permission.Resource != "finance" {
			t.Fatalf("财务特批权限元数据不符: %+v", permission)
		}
	}
	if !declared {
		t.Fatalf("%s 未在目录声明", override)
	}
	roles := map[string]map[string]bool{}
	for _, role := range manifest.Roles {
		roles[role.Code] = map[string]bool{}
		for _, granted := range role.Permissions {
			roles[role.Code][granted] = true
		}
	}
	if !roles["settlement_finance_manager"][override] {
		t.Fatal("财务经理缺少 settlement.finance.override")
	}
	for _, code := range []string{"settlement_sales", "settlement_project_assistant", "settlement_accountant", "settlement_reviewer", "settlement_management", "settlement_system_admin"} {
		if roles[code][override] {
			t.Fatalf("%s 不得持有 settlement.finance.override", code)
		}
	}
}
