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

// TestUnrestrictedRequiresWholeTenantReportScope 验证报表读取/导出只在
// APPLICATION / ENVIRONMENT / TENANT 范围下视为全范围授权。
// 参数：t 为测试上下文。
// 返回值：无，受限报表角色被判为全范围或缺失权限被放行时失败。
func TestUnrestrictedRequiresWholeTenantReportScope(t *testing.T) {
	restricted := Principal{
		UserID:         "manager-a",
		Roles:          []string{"settlement_management"},
		Permissions:    map[string]bool{"settlement.report.read": true},
		DataScopes:     []DataScope{{RoleCode: "settlement_management", ScopeType: "CONTRACT", ScopeID: "contract-1"}},
		CatalogVersion: "4",
	}
	if restricted.Unrestricted("settlement.report.read") {
		t.Fatal("受限 management 角色被判定为报表全范围")
	}
	application := restricted
	application.DataScopes = []DataScope{{RoleCode: "settlement_management", ScopeType: "APPLICATION"}}
	if !application.Unrestricted("settlement.report.read") {
		t.Fatal("APPLICATION 范围应允许报表读取")
	}
	// 报表读取不能顺带扩大导出能力：目录没有授予时仍为假。
	if application.Unrestricted("settlement.report.export") {
		t.Fatal("报表读取扩大了导出范围")
	}
	// 采集员只有读取能力，且同样受范围约束。
	collector := Principal{UserID: "collector-a", Roles: []string{"settlement_collector"}, Permissions: map[string]bool{"settlement.report.read": true}, DataScopes: []DataScope{{RoleCode: "settlement_collector", ScopeType: "PROJECT", ScopeID: "project-1"}}, CatalogVersion: "4"}
	if collector.Unrestricted("settlement.report.read") {
		t.Fatal("受限采集员被判定为报表全范围")
	}
	// 未版本化的内部组件/服务测试主体保留既有兼容语义。
	if !(Principal{Permissions: map[string]bool{"settlement.report.read": true}}).Unrestricted("settlement.report.read") {
		t.Fatal("未版本化主体兼容分支被破坏")
	}
}

// TestRequesterScopeSnapshotRoundTripsAuthorizationOnly 验证导出任务保存的申请人
// 范围快照只含范围判定字段、与回构主体等价，且不携带身份证明细与IP。
// 参数：t 为测试上下文。
// 返回值：无，快照泄漏敏感字段、丢失授权语义或回构不可重复时失败。
func TestRequesterScopeSnapshotRoundTripsAuthorizationOnly(t *testing.T) {
	original := Principal{
		TenantID: "tenant-a", UserID: "finance-a", IdentityID: "identity-a", PersonID: "person-a",
		DisplayName: "财务甲", Username: "finance.a", LoginIP: "203.0.113.9",
		Roles:          []string{"settlement_finance_manager"},
		Permissions:    map[string]bool{"settlement.report.read": true, "settlement.report.export": true, "settlement.invoice.read": false},
		DataScopes:     []DataScope{{RoleCode: "settlement_finance_manager", ScopeType: "TENANT"}},
		CatalogVersion: "4",
	}
	raw, err := NewRequesterScope(original).Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"tenant_id", "identity_id", "person_id", "display_name", "username", "login_ip", "203.0.113.9", "财务甲"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("范围快照泄漏 %s: %s", forbidden, raw)
		}
	}
	scope, err := DecodeRequesterScope(raw)
	if err != nil {
		t.Fatal(err)
	}
	restored := scope.Principal()
	if restored.UserID != "finance-a" || restored.CatalogVersion != "4" {
		t.Fatalf("快照丢失申请人或目录版本: %+v", restored)
	}
	if !restored.Unrestricted("settlement.report.export") {
		t.Fatal("全范围快照回构后失去导出能力")
	}
	if restored.Permissions["settlement.invoice.read"] {
		t.Fatal("快照保存了未授予的权限")
	}
	if len(restored.Roles) != 1 || restored.Roles[0] != "settlement_finance_manager" || len(restored.DataScopes) != 1 || restored.DataScopes[0].ScopeType != "TENANT" {
		t.Fatalf("快照授权字段不完整: %+v", restored)
	}
	// 回构主体只用于范围判定，不能当作可用的在线会话凭据。
	if restored.TenantID != "" || restored.IdentityID != "" || restored.PersonID != "" || restored.LoginIP != "" {
		t.Fatalf("回构主体携带了身份证明细: %+v", restored)
	}
	// 切片为副本：改动回构结果不得污染快照。
	restored.Roles[0] = "settlement_admin"
	restored.DataScopes[0].ScopeType = "TENANT"
	again, err := DecodeRequesterScope(raw)
	if err != nil || !again.Principal().Unrestricted("settlement.report.export") {
		t.Fatalf("快照被回构结果写回: %+v %v", again, err)
	}
	if again.Principal().Roles[0] != "settlement_finance_manager" {
		t.Fatalf("回构结果改写了快照角色: %+v", again)
	}
}

// TestDecodeRequesterScopeRejectsMissingOrMalformedSnapshot 验证空值与非法JSON
// 无法回构为可放行的范围，调用方必须据此失败关闭。
// 参数：t 为测试上下文。
// 返回值：无，空值/非法JSON被静默接受时失败。
func TestDecodeRequesterScopeRejectsMissingOrMalformedSnapshot(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("  \n"), []byte(`{"user_id":`)} {
		if _, err := DecodeRequesterScope(raw); err == nil {
			t.Fatalf("非法快照被接受: %q", raw)
		}
	}
	scope, err := DecodeRequesterScope([]byte(`null`))
	if err != nil || scope.UserID != "" {
		t.Fatalf("null 快照必须回构为空申请人: %+v %v", scope, err)
	}
}
