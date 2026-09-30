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

// AUD-2026-009：DevelopmentAuth 与公网明文 http origin 组合同样是生产可达特征
// （全部结算写接口免鉴权），必须在启动期被拒绝。
func TestLoadRejectsDevelopmentAuthWithPublicHTTPOrigin(t *testing.T) {
	t.Setenv("SETTLEMENT_MYSQL_DSN", "settlement:password@tcp(mysql:3306)/settlement")
	t.Setenv("SETTLEMENT_DEVELOPMENT_AUTH", "true")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "dev")
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "http://203.0.113.119:8081")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want DevelopmentAuth + public http origin rejection")
	}
	if !strings.Contains(err.Error(), "SETTLEMENT_DEVELOPMENT_AUTH") || !strings.Contains(err.Error(), "SETTLEMENT_PUBLIC_ORIGIN") {
		t.Fatalf("Load() error = %v, want explicit SETTLEMENT_DEVELOPMENT_AUTH/SETTLEMENT_PUBLIC_ORIGIN message", err)
	}
}

// AUD-2026-009：本机明文 http 是本地联调默认形态，必须保持放行（防止修复过拦）。
func TestLoadAllowsDevelopmentAuthOnLoopbackHTTPOrigin(t *testing.T) {
	t.Setenv("SETTLEMENT_MYSQL_DSN", "settlement:password@tcp(mysql:3306)/settlement")
	t.Setenv("SETTLEMENT_DEVELOPMENT_AUTH", "true")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "dev")
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "http://localhost:5173")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want loopback http to stay allowed for local debugging", err)
	}
	if !cfg.DevelopmentAuth {
		t.Fatal("DevelopmentAuth = false, want true")
	}
}

// AUD-2026-009：非 DevelopmentAuth 部署不受新增回环判断影响——公网 http origin
// 在显式开启 SETTLEMENT_ALLOW_INSECURE_HTTP_ORIGIN 时仍按既有语义放行。
func TestLoadKeepsNonDevelopmentAuthHTTPOverrideUnaffected(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "http://203.0.113.119:8081")
	t.Setenv("SETTLEMENT_ALLOW_INSECURE_HTTP_ORIGIN", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want non-development http override to keep working", err)
	}
	if cfg.DevelopmentAuth {
		t.Fatal("DevelopmentAuth = true, want false")
	}
}
