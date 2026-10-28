package service

import (
	"strings"
	"testing"
)

// TestRoleScopes 用途：验证角色并集不会用经营报表权限扩大开票范围。
// 参数：t 为测试上下文。
// 返回值：无，范围超出本人或负责项目时失败。
func TestRoleScopes(t *testing.T) {
	p := Principal{UserID: "sales-a", Roles: []string{"settlement_sales", "settlement_management"}, Permissions: map[string]bool{"settlement.invoice.request": true, "settlement.report.read": true}, CatalogVersion: "3"}
	if p.Unrestricted("settlement.invoice.request") {
		t.Fatal("报表角色扩大了开票范围")
	}
	q, args := p.ContractPredicate("settlement.invoice.request", "cs")
	if !strings.Contains(q, "owner_user_id") || len(args) != 1 || args[0] != "sales-a" {
		t.Fatalf("错误范围 %s %v", q, args)
	}
	p.Roles = []string{"settlement_project_assistant"}
	q, _ = p.ContractPredicate("settlement.invoice.request", "cs")
	if !strings.Contains(q, "settlement_project_assignment") {
		t.Fatal(q)
	}
	p.Roles = []string{"unknown"}
	q, _ = p.ContractPredicate("settlement.invoice.request", "cs")
	if q != "1=0" {
		t.Fatalf("未知角色获得访问: %s", q)
	}
}

// TestRolePermissionIsRequired 用途：验证角色与有效权限必须同时满足。
// 参数：t 为测试上下文。
// 返回值：无，缺少有效权限仍允许访问时失败。
func TestRolePermissionIsRequired(t *testing.T) {
	p := Principal{UserID: "u", Roles: []string{"settlement_admin"}, Permissions: map[string]bool{}}
	q, _ := p.ContractPredicate("settlement.invoice.request", "cs")
	if q != "1=0" {
		t.Fatal(q)
	}
}
