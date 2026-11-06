package config

import (
	"strings"
	"testing"
)

// TestNativeModeRejectsLeftoverContractIdentity 验证纯原生财务模式下残留合同
// 集成身份得到明确的模式冲突报错，而不是误导性的"缺机器验签材料"（C04）。
// 参数：t 为当前测试上下文。
// 返回值：无；残留身份被当作普通集成配置要求全套材料时失败。
func TestNativeModeRejectsLeftoverContractIdentity(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://settlement.example")
	t.Setenv("SETTLEMENT_NATIVE_FINANCIAL_ENABLED", "true")
	t.Setenv("SETTLEMENT_INTEGRATION_ENABLED", "false")
	t.Setenv("CONTRACT_INTEGRATION_CLIENT_ID", "settlement-contract")
	_, err := Load()
	if err == nil {
		t.Fatal("原生财务模式残留合同集成身份未被拒绝")
	}
	if !strings.Contains(err.Error(), "native financial mode") || !strings.Contains(err.Error(), "CONTRACT_INTEGRATION_CLIENT_ID") {
		t.Fatalf("错误未指向原生模式冲突: %v", err)
	}
}

// TestNativeAndIntegrationCoexistenceKeepsMachineIdentity 验证原生与集成同开的
// 共存模式不受模式冲突检查影响：全套机器身份齐备时正常加载。
// 参数：t 为当前测试上下文。
// 返回值：无；共存模式被误拒时失败。
func TestNativeAndIntegrationCoexistenceKeepsMachineIdentity(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://settlement.example")
	t.Setenv("SETTLEMENT_NATIVE_FINANCIAL_ENABLED", "true")
	t.Setenv("SETTLEMENT_INTEGRATION_ENABLED", "true")
	t.Setenv("PLATFORM_MACHINE_TOKEN_PUBLIC_KEY_PATH", "/test/platform-public.pem")
	t.Setenv("PLATFORM_MACHINE_TOKEN_ISSUER", "basic-platform")
	t.Setenv("CONTRACT_INTEGRATION_CLIENT_ID", "settlement-contract")
	t.Setenv("CONTRACT_INTEGRATION_AUDIENCE", "basic-platform")
	t.Setenv("CONTRACT_INTEGRATION_OAUTH_CLIENT_ID", "settlement-contract-worker")
	t.Setenv("CONTRACT_INTEGRATION_APPLICATION_ID", "1")
	t.Setenv("CONTRACT_INTEGRATION_ENVIRONMENT_ID", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("原生+集成共存模式被误拒: %v", err)
	}
	if !cfg.NativeFinancialEnabled || !cfg.IntegrationEnabled {
		t.Fatalf("共存模式开关被改写: native=%v integration=%v", cfg.NativeFinancialEnabled, cfg.IntegrationEnabled)
	}
}

// TestNonNativeLeftoverKeepsExistingValidation 验证非原生模式下残留合同身份
// 仍走既有"缺机器验签材料"校验，行为不变。
// 参数：t 为当前测试上下文。
// 返回值：无；非原生残留报错语义被改变时失败。
func TestNonNativeLeftoverKeepsExistingValidation(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://settlement.example")
	t.Setenv("SETTLEMENT_NATIVE_FINANCIAL_ENABLED", "false")
	t.Setenv("SETTLEMENT_INTEGRATION_ENABLED", "false")
	t.Setenv("CONTRACT_INTEGRATION_CLIENT_ID", "settlement-contract")
	_, err := Load()
	if err == nil {
		t.Fatal("非原生模式下残留身份未被既有校验拒绝")
	}
	if !strings.Contains(err.Error(), "is required for platform machine token verification") {
		t.Fatalf("非原生模式报错语义发生变化: %v", err)
	}
}
