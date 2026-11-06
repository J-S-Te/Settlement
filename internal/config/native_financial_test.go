package config

import "testing"

// TestNativeFinancialSwitchRejectsInvalidFlag 验证本地财务能力必须显式合法配置。
// 参数：t 为当前测试上下文。
// 返回值：无；非法开关被忽略时失败。
func TestNativeFinancialSwitchRejectsInvalidFlag(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://settlement.example")
	t.Setenv("SETTLEMENT_NATIVE_FINANCIAL_ENABLED", "not-a-bool")
	if _, err := Load(); err == nil {
		t.Fatal("本地财务能力非法开关未被拒绝")
	}
}

// TestNativeFinancialDoesNotRequireContractIdentity 验证公共平台验签配置不强迫采购合同系统。
// 参数：t 为当前测试上下文。
// 返回值：无；关合同仍要求其机器身份时失败。
func TestNativeFinancialDoesNotRequireContractIdentity(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://settlement.example")
	t.Setenv("SETTLEMENT_INTEGRATION_ENABLED", "false")
	t.Setenv("SETTLEMENT_NATIVE_FINANCIAL_ENABLED", "true")
	t.Setenv("PLATFORM_MACHINE_TOKEN_PUBLIC_KEY_PATH", "/test/platform-public.pem")
	t.Setenv("PLATFORM_MACHINE_TOKEN_ISSUER", "basic-platform")
	for _, key := range []string{"CONTRACT_INTEGRATION_CLIENT_ID", "CONTRACT_INTEGRATION_AUDIENCE", "CONTRACT_INTEGRATION_OAUTH_CLIENT_ID", "CONTRACT_INTEGRATION_APPLICATION_ID", "CONTRACT_INTEGRATION_ENVIRONMENT_ID"} {
		t.Setenv(key, "")
	}
	if cfg, err := Load(); err != nil || !cfg.NativeFinancialEnabled {
		t.Fatalf("无合同的显式本地财务配置被拒绝: %v", err)
	}
}

// TestNativeFinancialPreservesLocalContractDevelopment 验证既有本机合同Bearer模式不受选配调整破坏。
// 参数：t 为测试上下文。
// 返回值：无；在原本合法的本机配置上启动失败时失败。
func TestNativeFinancialPreservesLocalContractDevelopment(t *testing.T) {
	setValidProductionEnvironment(t)
	for key, value := range map[string]string{"SETTLEMENT_DEVELOPMENT_AUTH": "true", "SETTLEMENT_PUBLIC_ORIGIN": "http://localhost:5173", "PLATFORM_ENVIRONMENT_CODE": "dev", "APP_ENV": "development", "SETTLEMENT_INTEGRATION_ENABLED": "true", "SETTLEMENT_INTEGRATION_BEARER_TOKEN": "local-test-token", "PLATFORM_MACHINE_TOKEN_PUBLIC_KEY_PATH": "", "PLATFORM_MACHINE_TOKEN_ISSUER": "", "CONTRACT_INTEGRATION_CLIENT_ID": "", "CONTRACT_INTEGRATION_AUDIENCE": "", "CONTRACT_INTEGRATION_OAUTH_CLIENT_ID": "", "CONTRACT_INTEGRATION_APPLICATION_ID": "", "CONTRACT_INTEGRATION_ENVIRONMENT_ID": ""} {
		t.Setenv(key, value)
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}
