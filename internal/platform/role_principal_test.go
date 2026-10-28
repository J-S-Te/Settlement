package platform

import "testing"

// TestOnlinePrincipalMissingRolesClosesScope 用途：拒绝将旧在线元数据缺失解释为内部测试身份。
// 参数：t为测试上下文。
// 返回值：无，缺失角色仍有财务权限时失败。
func TestOnlinePrincipalMissingRolesClosesScope(t *testing.T) {
	p := principal(authorizationContext{TenantID: "t", SubjectID: "u", IdentityID: "u", Permissions: []string{"settlement.invoice.request", "settlement.receipt.record"}}, oidcClaims{})
	if p.CatalogVersion == "" || p.Allows("settlement.receipt.record") || p.Unrestricted("settlement.invoice.request") {
		t.Fatal("缺失在线角色获得全量权限")
	}
}
