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
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "http://203.0.113.119:8081")
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

// TestAuditDestinationRequiredResolution 覆盖 BIZ-XAPI-10 的判定顺序：
// 显式开关优先，其次 APP_ENV=production，最后子系统生产清单的 PLATFORM_ENVIRONMENT_CODE=prod。
func TestAuditDestinationRequiredResolution(t *testing.T) {
	tests := []struct {
		name            string
		explicit        string
		appEnv          string
		environmentCode string
		want            bool
	}{
		{name: "explicit true wins over dev app env", explicit: "true", appEnv: "development", environmentCode: "dev", want: true},
		{name: "explicit false wins over production environment code", explicit: "false", appEnv: "", environmentCode: "prod", want: false},
		{name: "app env production makes audit required", appEnv: "production", environmentCode: "dev", want: true},
		{name: "app env non production wins over prod environment code", appEnv: "development", environmentCode: "prod", want: false},
		{name: "production environment code fallback", environmentCode: "prod", want: true},
		{name: "production environment code alias", environmentCode: "production", want: true},
		{name: "dev is not required", environmentCode: "dev", want: false},
		{name: "empty defaults to not required", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := AuditDestinationRequired(test.explicit, test.appEnv, test.environmentCode)
			if err != nil {
				t.Fatalf("AuditDestinationRequired() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("AuditDestinationRequired(%q,%q,%q) = %v, want %v", test.explicit, test.appEnv, test.environmentCode, got, test.want)
			}
		})
	}
	if _, err := AuditDestinationRequired("not-a-bool", "", "prod"); err == nil {
		t.Fatal("invalid explicit switch must fail closed")
	}
}

// TestLoadRegistersIntegrationDestinationState 确认配置只登记目的地是否配置，
// 不因 API 进程缺少审计凭据而退出；生产 fail-closed 由 worker 负责。
func TestLoadRegistersIntegrationDestinationState(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
	t.Setenv("APP_ENV", "")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "prod")
	t.Setenv("SETTLEMENT_AUDIT_REQUIRED", "")
	t.Setenv("SETTLEMENT_AUDIT_CLIENT_ID", "settlement-audit")
	t.Setenv("SETTLEMENT_AUDIT_CLIENT_SECRET", "audit-secret")
	t.Setenv("SETTLEMENT_NOTIFICATION_CLIENT_ID", "")
	t.Setenv("SETTLEMENT_NOTIFICATION_CLIENT_SECRET", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.AuditDestinationRequired {
		t.Fatal("AuditDestinationRequired = false, want true for production environment")
	}
	if !cfg.AuditDestinationConfigured {
		t.Fatal("AuditDestinationConfigured = false, want true when both credentials exist")
	}
	if cfg.NotificationDestinationConfigured {
		t.Fatal("NotificationDestinationConfigured = true, want false when credentials are missing")
	}
}

// TestLoadHonoursExplicitAuditSwitchOutsideProduction 确认显式开关优先且非生产缺失不阻塞 Load。
func TestLoadHonoursExplicitAuditSwitchOutsideProduction(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("SETTLEMENT_PUBLIC_ORIGIN", "https://platform.example.com")
	t.Setenv("APP_ENV", "development")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "prod")
	t.Setenv("SETTLEMENT_AUDIT_REQUIRED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AuditDestinationRequired {
		t.Fatal("AuditDestinationRequired = true, want explicit false to win")
	}
	if cfg.AuditDestinationConfigured {
		t.Fatal("AuditDestinationConfigured = true, want false without credentials")
	}
}

// TestLoadRejectsInvalidAuditSwitch 确认非法显式开关不会被静默忽略。
func TestLoadRejectsInvalidAuditSwitch(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("APP_ENV", "")
	t.Setenv("SETTLEMENT_AUDIT_REQUIRED", "yes-please")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SETTLEMENT_AUDIT_REQUIRED") {
		t.Fatalf("Load() error = %v, want SETTLEMENT_AUDIT_REQUIRED rejection", err)
	}
}
