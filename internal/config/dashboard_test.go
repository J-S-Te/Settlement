package config

import (
	"testing"
)

// TestDashboardConfigurationFailsClosedWithoutBreakingOptionalStartup 验证可选用途配置。
// 参数：t为测试上下文。
// 返回值：无，启用残缺用途或错误信任绑定时失败。
func TestDashboardConfigurationFailsClosedWithoutBreakingOptionalStartup(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
	t.Setenv("SETTLEMENT_DASHBOARD_MACHINE_ENABLED", "false")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SETTLEMENT_DASHBOARD_MACHINE_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("accepted incomplete dashboard trust")
	}
	for suffix, value := range map[string]string{"CLIENT_ID": "dashboard-subject", "OAUTH_CLIENT_ID": "registered-oauth-id", "AUDIENCE": "basic-platform-application", "ISSUER": "basic-platform", "PUBLIC_KEY_PATH": "/synthetic/public.pem", "CALLER_APPLICATION_CODE": "data_analysis", "CALLER_ENVIRONMENT_CODE": "prod", "REQUIRED_SCOPE": "dashboard.settlement.read"} {
		t.Setenv("SETTLEMENT_DASHBOARD_MACHINE_"+suffix, value)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.DashboardMachineEnabled || c.DashboardMachineOAuthClientID != "registered-oauth-id" {
		t.Fatal("dashboard config not loaded")
	}
	t.Setenv("SETTLEMENT_DASHBOARD_MACHINE_REQUIRED_SCOPE", "settlement.contract.ingest")
	if _, err := Load(); err == nil {
		t.Fatal("accepted ingestion scope")
	}
	t.Setenv("SETTLEMENT_DASHBOARD_MACHINE_REQUIRED_SCOPE", "dashboard.settlement.read")
	t.Setenv("SETTLEMENT_DASHBOARD_MACHINE_CALLER_APPLICATION_CODE", "contract_management")
	if _, err := Load(); err == nil {
		t.Fatal("accepted contract caller")
	}
}
