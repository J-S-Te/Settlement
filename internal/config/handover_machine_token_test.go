package config

import (
	"strings"
	"testing"
)

// handoverMachineEnvironment 按平台清单写入交接机器的完整信任域：
// 平台自身 ApplicationJWTManager 签发（caller=platform/prod、唯一 scope=handover.machine），
// audience 为平台 application JWT 的 basic-platform-application。
func handoverMachineEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("SETTLEMENT_HANDOVER_ENABLED", "true")
	t.Setenv("SETTLEMENT_HANDOVER_CLIENT_ID", "platform-handover")
	t.Setenv("SETTLEMENT_HANDOVER_AUDIENCE", "basic-platform-application")
	t.Setenv("SETTLEMENT_HANDOVER_ISSUER", "basic-platform")
	t.Setenv("SETTLEMENT_HANDOVER_PUBLIC_KEY_PATH", "/app/data/keys/jwt-ed25519-public.pem")
	t.Setenv("SETTLEMENT_HANDOVER_APPLICATION_CODE", "platform")
	t.Setenv("SETTLEMENT_HANDOVER_SCOPE", "handover.machine")
	t.Setenv("SETTLEMENT_HANDOVER_CALLER_ENVIRONMENT_CODE", "prod")
}

func TestLoadDisablesHandoverMachineByDefault(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.HandoverMachineEnabled {
		t.Fatalf("handover machine enabled by default: %+v", c)
	}
}

func TestLoadAcceptsCompleteHandoverMachineBinding(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
	handoverMachineEnvironment(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !c.HandoverMachineEnabled ||
		c.HandoverMachineClientID != "platform-handover" ||
		c.HandoverMachineAudience != "basic-platform-application" ||
		c.HandoverMachineIssuer != "basic-platform" ||
		c.HandoverMachinePublicKeyPath != "/app/data/keys/jwt-ed25519-public.pem" ||
		c.HandoverMachineApplicationCode != "platform" ||
		c.HandoverMachineScope != "handover.machine" ||
		c.HandoverMachineCallerEnvironmentCode != "prod" {
		t.Fatalf("handover machine binding = %+v", c)
	}
}

// 平台 application JWT 验签器要求 issuer/audience/public key/client/caller/scope 齐全；
// 缺项必须在启动期失败关闭，不能退化为只验签不校验调用方。
// APPLICATION_CODE 与 SCOPE 有契约固定默认值（platform / handover.machine），
// 因此单独断言其默认值而不是“缺项报错”。
func TestLoadRejectsIncompleteHandoverMachineBinding(t *testing.T) {
	for _, missing := range []string{
		"SETTLEMENT_HANDOVER_CLIENT_ID",
		"SETTLEMENT_HANDOVER_AUDIENCE",
		"SETTLEMENT_HANDOVER_ISSUER",
		"SETTLEMENT_HANDOVER_PUBLIC_KEY_PATH",
		"SETTLEMENT_HANDOVER_CALLER_ENVIRONMENT_CODE",
		"OIDC_TENANT_ID",
	} {
		t.Run(missing, func(t *testing.T) {
			setValidProductionEnvironment(t)
			t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
			handoverMachineEnvironment(t)
			t.Setenv(missing, "")

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("Load() error = %v, want missing %s error", err, missing)
			}
		})
	}
}

// 调用方应用与 scope 由跨系统契约固定：未显式配置时也必须落在平台交接信任域。
func TestLoadDefaultsHandoverCallerApplicationAndScope(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
	handoverMachineEnvironment(t)
	t.Setenv("SETTLEMENT_HANDOVER_APPLICATION_CODE", "")
	t.Setenv("SETTLEMENT_HANDOVER_SCOPE", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.HandoverMachineApplicationCode != "platform" || c.HandoverMachineScope != "handover.machine" {
		t.Fatalf("handover caller app/scope = %q/%q, want platform/handover.machine", c.HandoverMachineApplicationCode, c.HandoverMachineScope)
	}
}

// 调用方环境默认取 PLATFORM_ENVIRONMENT_CODE，生产清单仍可用专用键覆盖。
func TestLoadHandoverCallerEnvironmentFallsBackToPlatformEnvironment(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
	handoverMachineEnvironment(t)
	t.Setenv("SETTLEMENT_HANDOVER_CALLER_ENVIRONMENT_CODE", "")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "prod")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.HandoverMachineCallerEnvironmentCode != "prod" {
		t.Fatalf("handover caller environment = %q, want prod", c.HandoverMachineCallerEnvironmentCode)
	}
}
