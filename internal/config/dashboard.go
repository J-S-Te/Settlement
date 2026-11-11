package config

import "fmt"

// validateDashboardMachine 校验启用后的独立结算看板信任绑定。
// 参数：无，读取当前配置；关闭用途不要求新增部署字段。
// 返回值：启用用途但字段缺失或用途漂移时返回错误。
func (c Config) validateDashboardMachine() error {
	if !c.DashboardMachineEnabled {
		return nil
	}
	for name, value := range map[string]string{
		"CLIENT_ID": c.DashboardMachineClientID, "OAUTH_CLIENT_ID": c.DashboardMachineOAuthClientID,
		"AUDIENCE": c.DashboardMachineAudience, "ISSUER": c.DashboardMachineIssuer,
		"PUBLIC_KEY_PATH":         c.DashboardMachinePublicKeyPath,
		"CALLER_APPLICATION_CODE": c.DashboardMachineCallerApplicationCode,
		"CALLER_ENVIRONMENT_CODE": c.DashboardMachineCallerEnvironmentCode,
		"REQUIRED_SCOPE":          c.DashboardMachineRequiredScope,
	} {
		if value == "" {
			return fmt.Errorf("SETTLEMENT_DASHBOARD_MACHINE_%s is required when enabled", name)
		}
	}
	if c.OIDCTenantID == "" {
		return fmt.Errorf("OIDC_TENANT_ID is required for dashboard machine identity")
	}
	if c.DashboardMachineCallerApplicationCode != "data_analysis" || c.DashboardMachineRequiredScope != "dashboard.settlement.read" {
		return fmt.Errorf("SETTLEMENT_DASHBOARD_MACHINE_CALLER_APPLICATION_CODE must be data_analysis and REQUIRED_SCOPE must be dashboard.settlement.read")
	}
	return nil
}
