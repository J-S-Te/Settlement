package config

import (
	"strings"
	"testing"
)

// SEC-D10：DevelopmentAuth 与生产环境码组合必须在启动期被拒绝。
func TestLoadRejectsDevelopmentAuthWithProductionEnvironmentCode(t *testing.T) {
	t.Setenv("SETTLEMENT_MYSQL_DSN", "settlement:password@tcp(mysql:3306)/settlement")
	t.Setenv("SETTLEMENT_DEVELOPMENT_AUTH", "true")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "prod")
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "http://localhost:5173")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want DevelopmentAuth + prod rejection")
	}
	if !strings.Contains(err.Error(), "SETTLEMENT_DEVELOPMENT_AUTH") || !strings.Contains(err.Error(), "PLATFORM_ENVIRONMENT_CODE=prod") {
		t.Fatalf("Load() error = %v, want explicit SETTLEMENT_DEVELOPMENT_AUTH/prod message", err)
	}
}

// SEC-D10：DevelopmentAuth 与公网 https 公网 origin 组合必须在启动期被拒绝
// （本地 .env.local 被拷进生产容器的典型事故形态）。
func TestLoadRejectsDevelopmentAuthWithPublicHTTPSOrigin(t *testing.T) {
	t.Setenv("SETTLEMENT_MYSQL_DSN", "settlement:password@tcp(mysql:3306)/settlement")
	t.Setenv("SETTLEMENT_DEVELOPMENT_AUTH", "true")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "dev")
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want DevelopmentAuth + public https origin rejection")
	}
	if !strings.Contains(err.Error(), "SETTLEMENT_DEVELOPMENT_AUTH") || !strings.Contains(err.Error(), "SETTLEMENT_PUBLIC_ORIGIN") {
		t.Fatalf("Load() error = %v, want explicit SETTLEMENT_DEVELOPMENT_AUTH/SETTLEMENT_PUBLIC_ORIGIN message", err)
	}
}

// SEC-D10：本机 https（自签调试）不属于生产特征，DevelopmentAuth 仍可启动。
func TestLoadAllowsDevelopmentAuthOnLoopbackHTTPSOrigin(t *testing.T) {
	t.Setenv("SETTLEMENT_MYSQL_DSN", "settlement:password@tcp(mysql:3306)/settlement")
	t.Setenv("SETTLEMENT_DEVELOPMENT_AUTH", "true")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "dev")
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://localhost:8443")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want loopback https to stay allowed for local debugging", err)
	}
	if !cfg.DevelopmentAuth {
		t.Fatal("DevelopmentAuth = false, want true")
	}
}
