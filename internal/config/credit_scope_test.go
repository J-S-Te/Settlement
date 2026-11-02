package config

import (
	"strings"
	"testing"
)

// TestLoadDefaultsCRMCreditScopeToCRMPermission 验证结算默认 scope 与 CRM 机器权限一致。
// 参数：t为测试上下文，使用独立环境变量夹具。
// 返回值：无；默认值偏离 customer.credit.payment.ingest 时测试失败。
func TestLoadDefaultsCRMCreditScopeToCRMPermission(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.CRMCreditScope != CRMCreditPaymentIngestScope {
		t.Fatalf("CRMCreditScope = %q, want %q", cfg.CRMCreditScope, CRMCreditPaymentIngestScope)
	}
	if CRMCreditPaymentIngestScope != "customer.credit.payment.ingest" {
		t.Fatalf("canonical scope changed: %q", CRMCreditPaymentIngestScope)
	}
	if cfg.CRMCreditSyncEnabled {
		t.Fatal("CRM credit sync must stay disabled by default")
	}
}

// TestLoadRejectsDriftedCRMCreditScope 验证历史 scope 名字在启动期失败关闭，
// 避免再次出现“启用即 401”的静默漂移。
// 参数：t为测试上下文，使用独立环境变量夹具。
// 返回值：无；错误 scope 未被拒绝时测试失败。
func TestLoadRejectsDriftedCRMCreditScope(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("CRM_CREDIT_SCOPE", "crm.credit.payment.ingest")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "CRM_CREDIT_SCOPE") {
		t.Fatalf("Load() error = %v, want CRM_CREDIT_SCOPE drift rejection", err)
	}
}

// TestLoadRejectsDriftedCRMCreditScopeEvenWhenSyncDisabled 验证 scope 校验不依赖开关：
// 关闭状态的旧环境文件同样必须失败关闭，不能把漂移带进后续版本。
// 参数：t为测试上下文，使用独立环境变量夹具。
// 返回值：无；关闭状态下放行错误 scope 时测试失败。
func TestLoadRejectsDriftedCRMCreditScopeEvenWhenSyncDisabled(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_CRM_CREDIT_SYNC_ENABLED", "false")
	t.Setenv("CRM_CREDIT_SCOPE", "crm.credit.payment.ingest")

	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted a drifted scope while CRM credit sync is disabled")
	}
}

// TestLoadAcceptsCanonicalScopeWhenSyncEnabled 验证启用同步时规范 scope 仍被接受。
// 参数：t为测试上下文，使用独立环境变量夹具。
// 返回值：无；规范配置被拒绝或字段丢失时测试失败。
func TestLoadAcceptsCanonicalScopeWhenSyncEnabled(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
	t.Setenv("SETTLEMENT_CRM_CREDIT_SYNC_ENABLED", "true")
	t.Setenv("CRM_CREDIT_SYNC_ENDPOINT", "http://customer-api:8090/api/v1/internal/credit/payment-events")
	t.Setenv("CRM_CREDIT_CLIENT_ID", "settlement-prod-crm-credit-ingest")
	t.Setenv("CRM_CREDIT_CLIENT_SECRET", "test-secret")
	t.Setenv("CRM_CREDIT_SCOPE", CRMCreditPaymentIngestScope)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.CRMCreditSyncEnabled || cfg.CRMCreditTokenEndpoint != "http://platform-api:8080/oauth2/token" {
		t.Fatalf("credit sync wiring = enabled:%v token:%q", cfg.CRMCreditSyncEnabled, cfg.CRMCreditTokenEndpoint)
	}
}
