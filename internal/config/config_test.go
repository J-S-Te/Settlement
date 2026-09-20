package config

import (
	"strings"
	"testing"
)

func TestLoadRejectsProductionHTTPOriginByDefault(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "http://platform.example.com")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SETTLEMENT_ALLOW_INSECURE_HTTP_ORIGIN=true") {
		t.Fatalf("Load() error = %v, want insecure HTTP origin rejection", err)
	}
}

func TestLoadAllowsProductionHTTPOriginWhenExplicitlyEnabled(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "http://47.111.20.119:8081")
	t.Setenv("SETTLEMENT_ALLOW_INSECURE_HTTP_ORIGIN", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.AllowInsecureHTTPOrigin {
		t.Fatal("AllowInsecureHTTPOrigin = false, want true")
	}
}

func TestLoadAcceptsProductionHTTPSOriginWithoutOverride(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")

	if _, err := Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}

func setValidProductionEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("SETTLEMENT_MYSQL_DSN", "settlement:password@tcp(mysql:3306)/settlement")
	t.Setenv("SETTLEMENT_DEVELOPMENT_AUTH", "false")
	t.Setenv("SETTLEMENT_ALLOW_INSECURE_HTTP_ORIGIN", "false")
	t.Setenv("PLATFORM_BASE_URL", "http://platform-api:8080")
	t.Setenv("OIDC_ISSUER", "https://identity.example.com/realms/basic-platform")
	t.Setenv("OIDC_CLIENT_ID", "settlement-prod-web")
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	t.Setenv("OIDC_REDIRECT_URI", "https://platform.example.com/settlement/auth/callback")
	t.Setenv("OIDC_TENANT_ID", "tenant-1")
	t.Setenv("OIDC_SESSION_ENCRYPTION_KEY_BASE64", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
}
