package config

import "testing"

// TestReimbursementCostSenderPinsPurpose 验证成本候选发送用途默认关闭且精确绑定当前租户环境。
// 参数：t为配置测试上下文。
// 返回值：无，缺配置或跨用途漂移被启用时失败。
func TestReimbursementCostSenderPinsPurpose(t *testing.T) {
	t.Setenv("SETTLEMENT_REIMBURSEMENT_COST_ENABLED", "")
	c := Config{OIDCTenantID: "tenant", OIDCEnvironmentCode: "local"}
	out, e := c.ReimbursementCosts()
	if e != nil || out.Enabled {
		t.Fatal("missing configuration must be disabled")
	}
	t.Setenv("SETTLEMENT_REIMBURSEMENT_COST_ENABLED", "true")
	if _, e = c.ReimbursementCosts(); e == nil {
		t.Fatal("enabled incomplete source accepted")
	}
	for key, value := range map[string]string{"URL": "http://reimbursement/internal/v1/settlement/cost-candidates", "TOKEN_URL": "http://platform/oauth2/token", "CLIENT_ID": "settlement-local-reimbursement-cost", "CLIENT_SECRET": "isolated-unique-not-a-real-secret", "SCOPE": "reimbursement.cost_candidate.read", "TENANT_ID": "tenant", "ENVIRONMENT": "local"} {
		t.Setenv("SETTLEMENT_REIMBURSEMENT_COST_"+key, value)
	}
	if out, e = c.ReimbursementCosts(); e != nil || !out.Enabled {
		t.Fatal(e)
	}
	t.Setenv("SETTLEMENT_REIMBURSEMENT_COST_SCOPE", "project.reference.read")
	if _, e = c.ReimbursementCosts(); e == nil {
		t.Fatal("accepted another machine purpose")
	}
}
