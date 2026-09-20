package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config intentionally keeps deployment secrets outside source control. The
// API is an independent subsystem and must not reuse another application's DB,
// browser cookie or machine-client credentials.
type Config struct {
	HTTPAddress                    string
	MySQLDSN                       string
	DevelopmentAuth                bool
	IntegrationEnabled             bool
	IntegrationBearerToken         string
	OutboxWorkerEnabled            bool
	OutboxWorkerBatchSize          int
	PlatformBaseURL                string
	OIDCIssuer                     string
	OIDCBackchannelBaseURL         string
	OIDCClientID                   string
	OIDCClientSecret               string
	OIDCRedirectURI                string
	OIDCPostLogoutURI              string
	OIDCTenantID                   string
	OIDCEnvironmentCode            string
	OIDCSessionCookieName          string
	OIDCSessionKey                 []byte
	OIDCSessionSecure              bool
	ContractClientID               string
	ContractAudience               string
	TaxResultIngestEnabled         bool
	TaxResultBearerToken           string
	TaxResultClientID              string
	TaxResultAudience              string
	TaxResultApplicationCode       string
	TaxProviderCode                string
	PersonnelDirectoryURL          string
	PersonnelDirectoryClientID     string
	PersonnelDirectoryClientSecret string
	CRMCreditSyncEnabled           bool
	CRMCreditEndpoint              string
	CRMCreditTokenEndpoint         string
	CRMCreditClientID              string
	CRMCreditClientSecret          string
	CRMCreditScope                 string
	FileGatewayMode                string
	FileGatewayURL                 string
	FileGatewayTokenURL            string
	FileGatewayClientID            string
	FileGatewayClientSecret        string
	FileGatewayScope               string
	FileGatewayApplicationID       string
	InvoiceIssuanceMode            string
	PublicOrigin                   string
	AllowInsecureHTTPOrigin        bool
	OIDCSessionIdleTTL             time.Duration
	OIDCSessionAbsoluteTTL         time.Duration
}

func Load() (Config, error) {
	c := Config{
		HTTPAddress:            env("SETTLEMENT_HTTP_ADDR", ":8085"),
		MySQLDSN:               strings.TrimSpace(os.Getenv("SETTLEMENT_MYSQL_DSN")),
		IntegrationBearerToken: strings.TrimSpace(os.Getenv("SETTLEMENT_INTEGRATION_BEARER_TOKEN")),
		PlatformBaseURL:        strings.TrimSpace(os.Getenv("PLATFORM_BASE_URL")), OIDCIssuer: strings.TrimSpace(os.Getenv("OIDC_ISSUER")), OIDCBackchannelBaseURL: strings.TrimSpace(os.Getenv("OIDC_BACKCHANNEL_BASE_URL")),
		OIDCClientID: strings.TrimSpace(os.Getenv("OIDC_CLIENT_ID")), OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"), OIDCRedirectURI: strings.TrimSpace(os.Getenv("OIDC_REDIRECT_URI")), OIDCPostLogoutURI: strings.TrimSpace(os.Getenv("OIDC_POST_LOGOUT_REDIRECT_URI")),
		OIDCTenantID: strings.TrimSpace(os.Getenv("OIDC_TENANT_ID")), OIDCEnvironmentCode: env("PLATFORM_ENVIRONMENT_CODE", "dev"), OIDCSessionCookieName: env("OIDC_SESSION_COOKIE_NAME", "settlement_session"),
		ContractClientID: strings.TrimSpace(os.Getenv("CONTRACT_INTEGRATION_CLIENT_ID")), ContractAudience: strings.TrimSpace(os.Getenv("CONTRACT_INTEGRATION_AUDIENCE")),
		TaxResultBearerToken:       strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_RESULT_BEARER_TOKEN")),
		TaxResultClientID:          strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_RESULT_CLIENT_ID")),
		TaxResultAudience:          strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_RESULT_AUDIENCE")),
		TaxResultApplicationCode:   env("SETTLEMENT_TAX_RESULT_APPLICATION_CODE", "tax_adapter"),
		TaxProviderCode:            env("SETTLEMENT_TAX_PROVIDER_CODE", "default"),
		PersonnelDirectoryURL:      strings.TrimRight(strings.TrimSpace(os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_URL")), "/"),
		PersonnelDirectoryClientID: strings.TrimSpace(os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_CLIENT_ID")), PersonnelDirectoryClientSecret: os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_CLIENT_SECRET"),
		CRMCreditEndpoint:        strings.TrimSpace(os.Getenv("CRM_CREDIT_SYNC_ENDPOINT")),
		CRMCreditTokenEndpoint:   strings.TrimSpace(os.Getenv("CRM_CREDIT_TOKEN_URL")),
		CRMCreditClientID:        strings.TrimSpace(os.Getenv("CRM_CREDIT_CLIENT_ID")),
		CRMCreditClientSecret:    os.Getenv("CRM_CREDIT_CLIENT_SECRET"),
		CRMCreditScope:           env("CRM_CREDIT_SCOPE", "crm.credit.payment.ingest"),
		FileGatewayMode:          strings.ToLower(env("SETTLEMENT_FILE_GATEWAY_MODE", "disabled")),
		FileGatewayURL:           strings.TrimRight(strings.TrimSpace(os.Getenv("FILE_GATEWAY_URL")), "/"),
		FileGatewayTokenURL:      strings.TrimSpace(os.Getenv("FILE_GATEWAY_TOKEN_URL")),
		FileGatewayClientID:      strings.TrimSpace(os.Getenv("FILE_GATEWAY_CLIENT_ID")),
		FileGatewayClientSecret:  os.Getenv("FILE_GATEWAY_CLIENT_SECRET"),
		FileGatewayScope:         env("FILE_GATEWAY_SCOPE", "platform:file:upload platform:file:bind platform:file:download"),
		FileGatewayApplicationID: strings.TrimSpace(os.Getenv("SETTLEMENT_FILE_GATEWAY_APPLICATION_ID")),
		InvoiceIssuanceMode:      strings.ToLower(env("SETTLEMENT_INVOICE_ISSUANCE_MODE", "manual")),
		PublicOrigin:             env("SETTLEMENT_PUBLIC_ORIGIN", "http://localhost:5173"),
	}
	var err error
	if c.DevelopmentAuth, err = boolEnv("SETTLEMENT_DEVELOPMENT_AUTH", false); err != nil {
		return c, err
	}
	if c.IntegrationEnabled, err = boolEnv("SETTLEMENT_INTEGRATION_ENABLED", false); err != nil {
		return c, err
	}
	if c.TaxResultIngestEnabled, err = boolEnv("SETTLEMENT_TAX_RESULT_INGEST_ENABLED", false); err != nil {
		return c, err
	}
	if c.OutboxWorkerEnabled, err = boolEnv("SETTLEMENT_OUTBOX_WORKER_ENABLED", true); err != nil {
		return c, err
	}
	if c.CRMCreditSyncEnabled, err = boolEnv("SETTLEMENT_CRM_CREDIT_SYNC_ENABLED", false); err != nil {
		return c, err
	}
	if c.OIDCSessionSecure, err = boolEnv("OIDC_SESSION_COOKIE_SECURE", true); err != nil {
		return c, err
	}
	if c.AllowInsecureHTTPOrigin, err = boolEnv("SETTLEMENT_ALLOW_INSECURE_HTTP_ORIGIN", false); err != nil {
		return c, err
	}
	if c.OIDCSessionIdleTTL, err = durationEnv("OIDC_SESSION_IDLE_TTL", 30*time.Minute); err != nil {
		return c, err
	}
	if c.OIDCSessionAbsoluteTTL, err = durationEnv("OIDC_SESSION_ABSOLUTE_TTL", 8*time.Hour); err != nil {
		return c, err
	}
	if raw := strings.TrimSpace(os.Getenv("OIDC_SESSION_ENCRYPTION_KEY_BASE64")); raw != "" {
		c.OIDCSessionKey, err = base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return c, fmt.Errorf("OIDC_SESSION_ENCRYPTION_KEY_BASE64: %w", err)
		}
	}
	if c.OutboxWorkerBatchSize, err = intEnv("SETTLEMENT_OUTBOX_BATCH_SIZE", 50); err != nil {
		return c, err
	}
	if c.HTTPAddress == "" || c.MySQLDSN == "" {
		return c, fmt.Errorf("SETTLEMENT_HTTP_ADDR and SETTLEMENT_MYSQL_DSN are required")
	}
	if c.IntegrationEnabled && !c.DevelopmentAuth && (c.ContractClientID == "" || c.ContractAudience == "") {
		return c, fmt.Errorf("contract client ID and audience are required when production integration is enabled")
	}
	if c.IntegrationEnabled && c.DevelopmentAuth && c.IntegrationBearerToken == "" {
		return c, fmt.Errorf("development integration token is required when development integration is enabled")
	}
	if c.TaxResultIngestEnabled && !c.DevelopmentAuth && (c.TaxResultClientID == "" || c.TaxResultAudience == "" || c.TaxResultApplicationCode == "" || c.TaxProviderCode == "") {
		return c, fmt.Errorf("tax result client, audience, application and provider are required when tax result ingest is enabled")
	}
	if c.TaxResultIngestEnabled && c.DevelopmentAuth && c.TaxResultBearerToken == "" {
		return c, fmt.Errorf("development tax result token is required when tax result ingest is enabled")
	}
	if c.InvoiceIssuanceMode == "tax_adapter" && !c.TaxResultIngestEnabled {
		return c, fmt.Errorf("SETTLEMENT_TAX_RESULT_INGEST_ENABLED must be true in tax_adapter mode")
	}
	if c.CRMCreditSyncEnabled {
		for name, value := range map[string]string{"CRM_CREDIT_SYNC_ENDPOINT": c.CRMCreditEndpoint, "CRM_CREDIT_CLIENT_ID": c.CRMCreditClientID, "CRM_CREDIT_CLIENT_SECRET": c.CRMCreditClientSecret, "CRM_CREDIT_SCOPE": c.CRMCreditScope} {
			if value == "" {
				return c, fmt.Errorf("%s is required when SETTLEMENT_CRM_CREDIT_SYNC_ENABLED=true", name)
			}
		}
		if c.CRMCreditTokenEndpoint == "" {
			c.CRMCreditTokenEndpoint = strings.TrimRight(c.PlatformBaseURL, "/") + "/oauth2/token"
		}
	}
	if c.FileGatewayMode != "disabled" && c.FileGatewayMode != "legacy" && c.FileGatewayMode != "dual" && c.FileGatewayMode != "required" {
		return c, fmt.Errorf("SETTLEMENT_FILE_GATEWAY_MODE must be disabled, legacy, dual or required")
	}
	if c.InvoiceIssuanceMode != "manual" && c.InvoiceIssuanceMode != "tax_adapter" {
		return c, fmt.Errorf("SETTLEMENT_INVOICE_ISSUANCE_MODE must be manual or tax_adapter")
	}
	if c.FileGatewayMode == "required" {
		for name, value := range map[string]string{"FILE_GATEWAY_URL": c.FileGatewayURL, "FILE_GATEWAY_CLIENT_ID": c.FileGatewayClientID, "FILE_GATEWAY_CLIENT_SECRET": c.FileGatewayClientSecret, "FILE_GATEWAY_SCOPE": c.FileGatewayScope, "SETTLEMENT_FILE_GATEWAY_APPLICATION_ID": c.FileGatewayApplicationID} {
			if value == "" {
				return c, fmt.Errorf("%s is required when SETTLEMENT_FILE_GATEWAY_MODE=required", name)
			}
		}
	}
	if c.FileGatewayTokenURL == "" && c.PlatformBaseURL != "" {
		c.FileGatewayTokenURL = strings.TrimRight(c.PlatformBaseURL, "/") + "/oauth2/token"
	}
	if !c.DevelopmentAuth {
		for name, value := range map[string]string{"PLATFORM_BASE_URL": c.PlatformBaseURL, "OIDC_ISSUER": c.OIDCIssuer, "OIDC_CLIENT_ID": c.OIDCClientID, "OIDC_CLIENT_SECRET": c.OIDCClientSecret, "OIDC_REDIRECT_URI": c.OIDCRedirectURI, "OIDC_TENANT_ID": c.OIDCTenantID} {
			if value == "" {
				return c, fmt.Errorf("%s is required", name)
			}
		}
		if len(c.OIDCSessionKey) != 32 {
			return c, fmt.Errorf("OIDC_SESSION_ENCRYPTION_KEY_BASE64 must decode to 32 bytes")
		}
		if c.OIDCSessionIdleTTL <= 0 || c.OIDCSessionAbsoluteTTL <= c.OIDCSessionIdleTTL {
			return c, fmt.Errorf("OIDC session absolute TTL must be longer than positive idle TTL")
		}
		if !strings.HasPrefix(c.PublicOrigin, "https://") && !(c.AllowInsecureHTTPOrigin && strings.HasPrefix(c.PublicOrigin, "http://")) {
			return c, fmt.Errorf("SETTLEMENT_PUBLIC_ORIGIN must use https in production unless SETTLEMENT_ALLOW_INSECURE_HTTP_ORIGIN=true")
		}
	}
	if c.OutboxWorkerBatchSize < 1 || c.OutboxWorkerBatchSize > 200 {
		return c, fmt.Errorf("SETTLEMENT_OUTBOX_BATCH_SIZE must be between 1 and 200")
	}
	return c, nil
}

// LoadDatabase is deliberately narrow so the migration job does not require
// browser/OIDC secrets that it never uses.
func LoadDatabase() (string, error) {
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_MYSQL_DSN"))
	if dsn == "" {
		return "", fmt.Errorf("SETTLEMENT_MYSQL_DSN is required")
	}
	return dsn, nil
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func boolEnv(key string, fallback bool) (bool, error) {
	v := env(key, strconv.FormatBool(fallback))
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}
func intEnv(key string, fallback int) (int, error) {
	v := env(key, strconv.Itoa(fallback))
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}
func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := env(key, fallback.String())
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}
