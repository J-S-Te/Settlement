package httpapi

import (
	"net/http"
	"testing"
)

// TestReadinessCapabilitiesReportDisabledAsReady 验证 C14 口径：可选能力未启用
// 只反映在 capabilities.state=disabled，不把 /readyz 整体降级。
// 参数：t 为当前测试上下文。
// 返回值：无；未启用能力被当作降级时失败。
func TestReadinessCapabilitiesReportDisabledAsReady(t *testing.T) {
	snapshot := integrationSnapshot{
		AuditRequired:              true,
		AuditConfigured:            true,
		NotificationConfigured:     true,
		ContractIntegrationEnabled: false,
		NativeFinancialEnabled:     true,
		CreditSyncEnabled:          false,
		ProjectReferenceConfigured: false,
	}
	status, body := evaluateReadiness(snapshot)
	if status != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("evaluateReadiness() = %d %v, want 200 ready（可选未启用不得降级）", status, body)
	}
	capabilities, ok := body["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities 段缺失: %v", body)
	}
	contract, ok := contractCapability(capabilities, "contract_integration")
	if !ok || contract["state"] != "disabled" || contract["enabled"] != false {
		t.Fatalf("contract_integration = %v, want disabled", contract)
	}
	native, ok := contractCapability(capabilities, "native_financial")
	if !ok || native["state"] != "ready" {
		t.Fatalf("native_financial = %v, want ready", native)
	}
}

// TestReadinessCapabilitiesRenderEnabledStates 验证启用能力与核心降级的组合：
// 能力段照常输出 ready，核心审计缺失仍 503（C14：两类信号互不覆盖）。
// 参数：t 为当前测试上下文。
// 返回值：无；能力段被核心状态吞掉时失败。
func TestReadinessCapabilitiesRenderEnabledStates(t *testing.T) {
	snapshot := integrationSnapshot{
		AuditRequired:              true,
		AuditConfigured:            false,
		NotificationConfigured:     true,
		ContractIntegrationEnabled: true,
		NativeFinancialEnabled:     false,
		CreditSyncEnabled:          true,
		ProjectReferenceConfigured: true,
	}
	status, body := evaluateReadiness(snapshot)
	if status != http.StatusServiceUnavailable || body["status"] != "not_ready" {
		t.Fatalf("evaluateReadiness() = %d %v, want 503 not_ready", status, body)
	}
	capabilities, ok := body["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("503 响应仍应携带 capabilities 段: %v", body)
	}
	for name, want := range map[string]string{
		"contract_integration": "ready",
		"native_financial":     "disabled",
		"crm_credit_sync":      "ready",
		"project_reference":    "ready",
	} {
		capability, ok := contractCapability(capabilities, name)
		if !ok || capability["state"] != want {
			t.Fatalf("capabilities[%s] = %v, want state=%s", name, capability, want)
		}
	}
}

// contractCapability 从能力段取单项，缺失时报测试失败。
// 参数：capabilities 为能力段，name 为能力键。
// 返回值：能力项与是否存在。
func contractCapability(capabilities map[string]any, name string) (map[string]any, bool) {
	entry, ok := capabilities[name].(map[string]any)
	return entry, ok
}
