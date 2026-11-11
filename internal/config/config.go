package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// CRMCreditPaymentIngestScope 是结算→CRM 回款/冲销信用事实写入的唯一合法 scope。
// 它必须同时匹配 CRM 机器入口的权限白名单（customer_and_opportunity crmauth）与
// 平台受控用途 crm_credit_payment_ingest；历史上双方各用一个名字
// （crm.credit.payment.ingest 对 customer.credit.payment.ingest）导致“启用即 401”，
// 因此这里收敛为单一常量，并在启动期拒绝任何漂移。
const CRMCreditPaymentIngestScope = "customer.credit.payment.ingest"

// Config 有意将部署密钥置于源码控制之外。
// API 为独立子系统，不得复用其他应用的数据库、
// 浏览器 Cookie 或机器客户端凭据。
type Config struct {
	// DashboardMachineClientID绑定JWT sub；OAuthClientID绑定注册记录oauth_client_id。
	DashboardMachineEnabled               bool
	DashboardMachineClientID              string
	DashboardMachineOAuthClientID         string
	DashboardMachineAudience              string
	DashboardMachineIssuer                string
	DashboardMachinePublicKeyPath         string
	DashboardMachineCallerApplicationCode string
	DashboardMachineCallerEnvironmentCode string
	DashboardMachineRequiredScope         string

	CentralSessionEnabled        bool
	CentralSessionPublicBaseURL  string
	ProjectReferenceURL          string
	ProjectReferenceTokenURL     string
	ProjectReferenceClientID     string
	ProjectReferenceClientSecret string
	HTTPAddress                  string
	MySQLDSN                     string
	DevelopmentAuth              bool
	IntegrationEnabled           bool
	NativeFinancialEnabled       bool
	IntegrationBearerToken       string
	OutboxWorkerEnabled          bool
	OutboxWorkerBatchSize        int
	PlatformBaseURL              string
	OIDCIssuer                   string
	OIDCBackchannelBaseURL       string
	OIDCClientID                 string
	OIDCClientSecret             string
	OIDCRedirectURI              string
	OIDCPostLogoutURI            string
	OIDCTenantID                 string
	OIDCEnvironmentCode          string
	// TrustedProxies 是唯一允许采信转发头的直接对端（AUD-IP-2026，默认仅回环）。
	TrustedProxies                    []netip.Prefix
	OIDCSessionCookieName             string
	OIDCSessionKey                    []byte
	OIDCSessionSecure                 bool
	ContractClientID                  string
	ContractAudience                  string
	PlatformMachineTokenPublicKeyPath string
	PlatformMachineTokenIssuer        string
	ContractOAuthClientID             string
	ContractApplicationID             string
	ContractEnvironmentID             string
	TaxResultIngestEnabled            bool
	TaxResultBearerToken              string
	TaxResultClientID                 string
	TaxResultAudience                 string
	TaxResultApplicationCode          string
	// HandoverMachine 是基础平台人员交接机器协议的入站配置。
	// 信任域与平台 application JWT 对齐：issuer/audience/client_id/caller 应用与环境/
	// 唯一 scope 完整绑定，租户来自签名 claims，不再使用 OIDC 严格验签器。
	HandoverMachineEnabled               bool
	HandoverMachineClientID              string
	HandoverMachineAudience              string
	HandoverMachineApplicationCode       string
	HandoverMachineScope                 string
	HandoverMachineIssuer                string
	HandoverMachinePublicKeyPath         string
	HandoverMachineCallerEnvironmentCode string
	TaxProviderCode                      string
	PersonnelDirectoryURL                string
	PersonnelDirectoryClientID           string
	PersonnelDirectoryClientSecret       string
	CRMCreditSyncEnabled                 bool
	ContractImportEnabled                bool
	CRMCreditEndpoint                    string
	CRMCreditTokenEndpoint               string
	CRMCreditClientID                    string
	CRMCreditClientSecret                string
	CRMCreditScope                       string
	FileGatewayMode                      string
	FileGatewayURL                       string
	FileGatewayTokenURL                  string
	FileGatewayClientID                  string
	FileGatewayClientSecret              string
	FileGatewayScope                     string
	FileGatewayApplicationID             string
	InvoiceIssuanceMode                  string
	PublicOrigin                         string
	AllowInsecureHTTPOrigin              bool
	OIDCSessionIdleTTL                   time.Duration
	OIDCSessionAbsoluteTTL               time.Duration
	// AuditDestinationRequired 是 BIZ-XAPI-10 的审计目的地必开判定：
	// 显式 SETTLEMENT_AUDIT_REQUIRED 优先，否则 APP_ENV=production /
	// PLATFORM_ENVIRONMENT_CODE=prod 视为生产必开。必开但凭据不全时 worker 失败关闭。
	AuditDestinationRequired bool
	// AuditDestinationConfigured / NotificationDestinationConfigured 只表示机器凭据是否齐全，
	// 不输出凭据值；供 /readyz、/metrics 暴露“目的地缺失”而不是静默积压。
	AuditDestinationConfigured        bool
	NotificationDestinationConfigured bool
}

// AuditDestinationRequired 解析审计目的地是否必开（BIZ-XAPI-10）。
// 参数：explicit 为显式审计开关（生产调用点经 AuditRequiredSwitch 读取，兼容
// SETTLEMENT_AUDIT_REQUIRED 与平台统一键 PLATFORM_AUDIT_REQUIRED），appEnv 为 APP_ENV，
// environmentCode 为 PLATFORM_ENVIRONMENT_CODE。
// 返回值：生效判定；显式值非法时返回错误（失败关闭，不静默降级）。
// 判定顺序与平台 platformAuditRequired 同构：显式开关 > APP_ENV=production；
// 子系统生产清单只设置 PLATFORM_ENVIRONMENT_CODE=prod，因此最后回退到该口径。
func AuditDestinationRequired(explicit, appEnv, environmentCode string) (bool, error) {
	if raw := strings.TrimSpace(explicit); raw != "" {
		required, err := strconv.ParseBool(raw)
		if err != nil {
			// 非法值可能来自子系统键或平台统一键，两个键名都列出，避免调用点误导排障。
			return false, fmt.Errorf("SETTLEMENT_AUDIT_REQUIRED/PLATFORM_AUDIT_REQUIRED: %w", err)
		}
		return required, nil
	}
	if env := strings.TrimSpace(appEnv); env != "" {
		return strings.EqualFold(env, "production"), nil
	}
	return isProductionEnvironmentCode(environmentCode), nil
}

// AuditRequiredSwitch 返回审计必开的显式开关值：SETTLEMENT_AUDIT_REQUIRED 优先，
// 缺失时兼容平台统一键 PLATFORM_AUDIT_REQUIRED（生产 descriptor 统一声明该键）。
// 返回原始字符串，由 AuditDestinationRequired 负责解析与失败关闭。
func AuditRequiredSwitch() string {
	if raw := strings.TrimSpace(os.Getenv("SETTLEMENT_AUDIT_REQUIRED")); raw != "" {
		return raw
	}
	return strings.TrimSpace(os.Getenv("PLATFORM_AUDIT_REQUIRED"))
}

// isProductionEnvironmentCode 按平台环境编码判断生产：兼容 prod 与 production。
func isProductionEnvironmentCode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "prod", "production":
		return true
	default:
		return false
	}
}

// Load 读取并校验结算启动配置与各独立机器用途。
// 参数：无，从进程环境读取配置。
// 返回值：有效配置；依赖或受信身份缺失、设置非法时返回错误。
func Load() (Config, error) {
	trustedProxies, trustedErr := parseTrustedProxies(env("APP_TRUSTED_PROXIES", "127.0.0.1/32,::1/128"))
	if trustedErr != nil {
		return Config{}, fmt.Errorf("APP_TRUSTED_PROXIES: %w", trustedErr)
	}
	c := Config{
		DashboardMachineClientID:              strings.TrimSpace(os.Getenv("SETTLEMENT_DASHBOARD_MACHINE_CLIENT_ID")),
		DashboardMachineOAuthClientID:         strings.TrimSpace(os.Getenv("SETTLEMENT_DASHBOARD_MACHINE_OAUTH_CLIENT_ID")),
		DashboardMachineAudience:              strings.TrimSpace(os.Getenv("SETTLEMENT_DASHBOARD_MACHINE_AUDIENCE")),
		DashboardMachineIssuer:                strings.TrimSpace(os.Getenv("SETTLEMENT_DASHBOARD_MACHINE_ISSUER")),
		DashboardMachinePublicKeyPath:         strings.TrimSpace(os.Getenv("SETTLEMENT_DASHBOARD_MACHINE_PUBLIC_KEY_PATH")),
		DashboardMachineCallerApplicationCode: strings.TrimSpace(os.Getenv("SETTLEMENT_DASHBOARD_MACHINE_CALLER_APPLICATION_CODE")),
		DashboardMachineCallerEnvironmentCode: strings.TrimSpace(os.Getenv("SETTLEMENT_DASHBOARD_MACHINE_CALLER_ENVIRONMENT_CODE")),
		DashboardMachineRequiredScope:         strings.TrimSpace(os.Getenv("SETTLEMENT_DASHBOARD_MACHINE_REQUIRED_SCOPE")),

		ProjectReferenceURL:          strings.TrimSpace(os.Getenv("SETTLEMENT_PROJECT_REFERENCE_URL")),
		ProjectReferenceTokenURL:     strings.TrimSpace(os.Getenv("SETTLEMENT_PROJECT_REFERENCE_TOKEN_URL")),
		ProjectReferenceClientID:     strings.TrimSpace(os.Getenv("SETTLEMENT_PROJECT_REFERENCE_CLIENT_ID")),
		ProjectReferenceClientSecret: os.Getenv("SETTLEMENT_PROJECT_REFERENCE_CLIENT_SECRET"),
		HTTPAddress:                  env("SETTLEMENT_HTTP_ADDR", ":8085"),
		MySQLDSN:                     strings.TrimSpace(os.Getenv("SETTLEMENT_MYSQL_DSN")),
		IntegrationBearerToken:       strings.TrimSpace(os.Getenv("SETTLEMENT_INTEGRATION_BEARER_TOKEN")),
		PlatformBaseURL:              strings.TrimSpace(os.Getenv("PLATFORM_BASE_URL")), OIDCIssuer: strings.TrimSpace(os.Getenv("OIDC_ISSUER")), OIDCBackchannelBaseURL: strings.TrimSpace(os.Getenv("OIDC_BACKCHANNEL_BASE_URL")),
		OIDCClientID: strings.TrimSpace(os.Getenv("OIDC_CLIENT_ID")), OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"), OIDCRedirectURI: strings.TrimSpace(os.Getenv("OIDC_REDIRECT_URI")), OIDCPostLogoutURI: strings.TrimSpace(os.Getenv("OIDC_POST_LOGOUT_REDIRECT_URI")),
		OIDCTenantID: strings.TrimSpace(os.Getenv("OIDC_TENANT_ID")), OIDCEnvironmentCode: env("PLATFORM_ENVIRONMENT_CODE", "dev"),
		TrustedProxies: trustedProxies, OIDCSessionCookieName: env("OIDC_SESSION_COOKIE_NAME", "settlement_session"),
		ContractClientID: strings.TrimSpace(os.Getenv("CONTRACT_INTEGRATION_CLIENT_ID")), ContractAudience: strings.TrimSpace(os.Getenv("CONTRACT_INTEGRATION_AUDIENCE")),
		PlatformMachineTokenPublicKeyPath:    strings.TrimSpace(os.Getenv("PLATFORM_MACHINE_TOKEN_PUBLIC_KEY_PATH")),
		PlatformMachineTokenIssuer:           strings.TrimSpace(os.Getenv("PLATFORM_MACHINE_TOKEN_ISSUER")),
		ContractOAuthClientID:                strings.TrimSpace(os.Getenv("CONTRACT_INTEGRATION_OAUTH_CLIENT_ID")),
		ContractApplicationID:                strings.TrimSpace(os.Getenv("CONTRACT_INTEGRATION_APPLICATION_ID")),
		ContractEnvironmentID:                strings.TrimSpace(os.Getenv("CONTRACT_INTEGRATION_ENVIRONMENT_ID")),
		TaxResultBearerToken:                 strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_RESULT_BEARER_TOKEN")),
		TaxResultClientID:                    strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_RESULT_CLIENT_ID")),
		TaxResultAudience:                    strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_RESULT_AUDIENCE")),
		TaxResultApplicationCode:             env("SETTLEMENT_TAX_RESULT_APPLICATION_CODE", "tax_adapter"),
		HandoverMachineClientID:              strings.TrimSpace(os.Getenv("SETTLEMENT_HANDOVER_CLIENT_ID")),
		HandoverMachineAudience:              strings.TrimSpace(os.Getenv("SETTLEMENT_HANDOVER_AUDIENCE")),
		HandoverMachineApplicationCode:       env("SETTLEMENT_HANDOVER_APPLICATION_CODE", "platform"),
		HandoverMachineScope:                 env("SETTLEMENT_HANDOVER_SCOPE", "handover.machine"),
		HandoverMachineIssuer:                strings.TrimSpace(os.Getenv("SETTLEMENT_HANDOVER_ISSUER")),
		HandoverMachinePublicKeyPath:         strings.TrimSpace(os.Getenv("SETTLEMENT_HANDOVER_PUBLIC_KEY_PATH")),
		HandoverMachineCallerEnvironmentCode: env("SETTLEMENT_HANDOVER_CALLER_ENVIRONMENT_CODE", strings.TrimSpace(os.Getenv("PLATFORM_ENVIRONMENT_CODE"))),
		TaxProviderCode:                      env("SETTLEMENT_TAX_PROVIDER_CODE", "default"),
		PersonnelDirectoryURL:                strings.TrimRight(strings.TrimSpace(os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_URL")), "/"),
		PersonnelDirectoryClientID:           strings.TrimSpace(os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_CLIENT_ID")), PersonnelDirectoryClientSecret: os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_CLIENT_SECRET"),
		CRMCreditEndpoint:        strings.TrimSpace(os.Getenv("CRM_CREDIT_SYNC_ENDPOINT")),
		CRMCreditTokenEndpoint:   strings.TrimSpace(os.Getenv("CRM_CREDIT_TOKEN_URL")),
		CRMCreditClientID:        strings.TrimSpace(os.Getenv("CRM_CREDIT_CLIENT_ID")),
		CRMCreditClientSecret:    os.Getenv("CRM_CREDIT_CLIENT_SECRET"),
		CRMCreditScope:           env("CRM_CREDIT_SCOPE", CRMCreditPaymentIngestScope),
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
	if c.ProjectReferenceURL != "" || c.ProjectReferenceClientID != "" || c.ProjectReferenceClientSecret != "" || c.ProjectReferenceTokenURL != "" {
		if c.ProjectReferenceURL == "" || c.ProjectReferenceClientID == "" || c.ProjectReferenceClientSecret == "" || c.ProjectReferenceTokenURL == "" {
			return c, fmt.Errorf("project reference OAuth configuration is incomplete")
		}
	}
	// BIZ-XAPI-10：审计目的地是否必开按显式开关优先、生产环境兜底；
	// 目的地的凭据完整性只做布尔登记，供 /readyz 与 /metrics 暴露，不在此处退出
	// 进程（API 需要保持可探测；worker 负责生产 fail-closed）。
	auditDestinationRequired, auditRequiredErr := AuditDestinationRequired(
		AuditRequiredSwitch(),
		os.Getenv("APP_ENV"),
		c.OIDCEnvironmentCode,
	)
	if auditRequiredErr != nil {
		return c, auditRequiredErr
	}
	c.AuditDestinationRequired = auditDestinationRequired
	c.AuditDestinationConfigured = strings.TrimSpace(os.Getenv("SETTLEMENT_AUDIT_CLIENT_ID")) != "" &&
		strings.TrimSpace(os.Getenv("SETTLEMENT_AUDIT_CLIENT_SECRET")) != ""
	c.NotificationDestinationConfigured = strings.TrimSpace(os.Getenv("SETTLEMENT_NOTIFICATION_CLIENT_ID")) != "" &&
		strings.TrimSpace(os.Getenv("SETTLEMENT_NOTIFICATION_CLIENT_SECRET")) != ""
	var err error
	if c.DevelopmentAuth, err = boolEnv("SETTLEMENT_DEVELOPMENT_AUTH", false); err != nil {
		return c, err
	}
	// 安全理由（SEC-D10）：DevelopmentAuth 会把全部结算写操作退化为免鉴权的
	// dev-finance 全权限主体；它与生产特征组合（本地 .env.local 被拷进生产容器）
	// 必须在启动期拒绝，而不是依赖部署纪律。失败关闭：命中任一生产特征即报错。
	if c.DevelopmentAuth {
		if strings.EqualFold(strings.TrimSpace(c.OIDCEnvironmentCode), "prod") {
			return c, fmt.Errorf("SETTLEMENT_DEVELOPMENT_AUTH=true is forbidden when PLATFORM_ENVIRONMENT_CODE=prod: development auth bypasses every settlement authentication check")
		}
		// 安全理由（SEC-D10 + AUD-2026-009）：DevelopmentAuth 与任何公网入口
		// 组合都必须在启动期拒绝——除公网 https 外，明文 http 公网 origin 同样
		// 意味着免鉴权写接口可达生产网络。失败关闭：命中即报错。
		if origin := strings.ToLower(strings.TrimSpace(c.PublicOrigin)); (strings.HasPrefix(origin, "https://") || strings.HasPrefix(origin, "http://")) && !isLoopbackHTTPOrigin(origin) {
			return c, fmt.Errorf("SETTLEMENT_DEVELOPMENT_AUTH=true is forbidden when SETTLEMENT_PUBLIC_ORIGIN is a public non-loopback origin: development auth bypasses every settlement authentication check")
		}
	}
	if c.NativeFinancialEnabled, err = boolEnv("SETTLEMENT_NATIVE_FINANCIAL_ENABLED", false); err != nil {
		return c, err
	}
	if c.IntegrationEnabled, err = boolEnv("SETTLEMENT_INTEGRATION_ENABLED", false); err != nil {
		return c, err
	}
	if c.TaxResultIngestEnabled, err = boolEnv("SETTLEMENT_TAX_RESULT_INGEST_ENABLED", false); err != nil {
		return c, err
	}
	if c.DashboardMachineEnabled, err = boolEnv("SETTLEMENT_DASHBOARD_MACHINE_ENABLED", false); err != nil {
		return c, err
	}
	if err = c.validateDashboardMachine(); err != nil {
		return c, err
	}
	if c.HandoverMachineEnabled, err = boolEnv("SETTLEMENT_HANDOVER_ENABLED", false); err != nil {
		return c, err
	}
	if c.OutboxWorkerEnabled, err = boolEnv("SETTLEMENT_OUTBOX_WORKER_ENABLED", true); err != nil {
		return c, err
	}
	if c.ContractImportEnabled, err = boolEnv("SETTLEMENT_CONTRACT_IMPORT_ENABLED", false); err != nil {
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
	c.CentralSessionEnabled, err = boolEnv("CENTRAL_SESSION_ENABLED", false)
	if err != nil {
		return c, err
	}
	c.CentralSessionPublicBaseURL = strings.TrimSpace(os.Getenv("CENTRAL_SESSION_PUBLIC_BASE_URL"))
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
	// 原生财务模式（SETTLEMENT_NATIVE_FINANCIAL_ENABLED=true 且合同集成关闭）下
	// 不允许残留合同集成身份：半套残留会让后续集成启用难以核对，也要把明确的
	// 模式冲突与"缺机器验签材料"区分开。原生+集成同开的共存模式仍合法，本地
	// 基线与合同事实按 basis_type 并存，不受本检查影响。
	if c.NativeFinancialEnabled && !c.IntegrationEnabled {
		for name, value := range map[string]string{"CONTRACT_INTEGRATION_CLIENT_ID": c.ContractClientID, "CONTRACT_INTEGRATION_AUDIENCE": c.ContractAudience, "CONTRACT_INTEGRATION_OAUTH_CLIENT_ID": c.ContractOAuthClientID, "CONTRACT_INTEGRATION_APPLICATION_ID": c.ContractApplicationID, "CONTRACT_INTEGRATION_ENVIRONMENT_ID": c.ContractEnvironmentID} {
			if value != "" {
				return c, fmt.Errorf("%s must be empty in native financial mode (SETTLEMENT_NATIVE_FINANCIAL_ENABLED=true with SETTLEMENT_INTEGRATION_ENABLED=false); clear the leftover contract identity or re-enable settlement integration", name)
			}
		}
	}
	if (c.IntegrationEnabled && !c.DevelopmentAuth) || c.ContractClientID != "" || c.ContractAudience != "" || c.ContractOAuthClientID != "" || c.ContractApplicationID != "" || c.ContractEnvironmentID != "" {
		if c.DevelopmentAuth {
			return c, fmt.Errorf("platform machine tokens require production authentication")
		}
		for name, value := range map[string]string{"PLATFORM_MACHINE_TOKEN_PUBLIC_KEY_PATH": c.PlatformMachineTokenPublicKeyPath, "PLATFORM_MACHINE_TOKEN_ISSUER": c.PlatformMachineTokenIssuer, "CONTRACT_INTEGRATION_CLIENT_ID": c.ContractClientID, "CONTRACT_INTEGRATION_AUDIENCE": c.ContractAudience, "CONTRACT_INTEGRATION_OAUTH_CLIENT_ID": c.ContractOAuthClientID, "CONTRACT_INTEGRATION_APPLICATION_ID": c.ContractApplicationID, "CONTRACT_INTEGRATION_ENVIRONMENT_ID": c.ContractEnvironmentID, "OIDC_TENANT_ID": c.OIDCTenantID, "PLATFORM_ENVIRONMENT_CODE": c.OIDCEnvironmentCode} {
			if value == "" {
				return c, fmt.Errorf("%s is required for platform machine token verification", name)
			}
		}
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
	// 交接协议只服务平台编排器：启用后必须能用平台 Ed25519 公钥完成完整信任域验签，
	// 任一绑定缺项都在启动期失败关闭，不能退化为“只校验签名不校验调用方”。
	if c.HandoverMachineEnabled {
		for _, item := range []struct {
			name, value string
		}{
			{"SETTLEMENT_HANDOVER_CLIENT_ID", c.HandoverMachineClientID},
			{"SETTLEMENT_HANDOVER_AUDIENCE", c.HandoverMachineAudience},
			{"SETTLEMENT_HANDOVER_ISSUER", c.HandoverMachineIssuer},
			{"SETTLEMENT_HANDOVER_PUBLIC_KEY_PATH", c.HandoverMachinePublicKeyPath},
			{"SETTLEMENT_HANDOVER_APPLICATION_CODE", c.HandoverMachineApplicationCode},
			{"SETTLEMENT_HANDOVER_CALLER_ENVIRONMENT_CODE", c.HandoverMachineCallerEnvironmentCode},
			{"SETTLEMENT_HANDOVER_SCOPE", c.HandoverMachineScope},
			{"OIDC_TENANT_ID", c.OIDCTenantID},
		} {
			if strings.TrimSpace(item.value) == "" {
				return c, fmt.Errorf("%s is required when SETTLEMENT_HANDOVER_ENABLED=true", item.name)
			}
		}
	}
	if c.TaxResultIngestEnabled && c.DevelopmentAuth && c.TaxResultBearerToken == "" {
		return c, fmt.Errorf("development tax result token is required when tax result ingest is enabled")
	}
	if c.InvoiceIssuanceMode == "tax_adapter" && !c.TaxResultIngestEnabled {
		return c, fmt.Errorf("SETTLEMENT_TAX_RESULT_INGEST_ENABLED must be true in tax_adapter mode")
	}
	// scope 漂移必须在启动期失败关闭，而不是等到首次投递才 401：无论同步开关是否
	// 打开都校验，避免旧环境文件把已经被平台/CRM 拒绝的名字继续带进新版本。
	if c.CRMCreditScope != CRMCreditPaymentIngestScope {
		return c, fmt.Errorf("CRM_CREDIT_SCOPE must be %q to match the CRM machine permission and the platform crm_credit_payment_ingest purpose binding", CRMCreditPaymentIngestScope)
	}
	if c.CRMCreditSyncEnabled {
		for name, value := range map[string]string{"CRM_CREDIT_SYNC_ENDPOINT": c.CRMCreditEndpoint, "CRM_CREDIT_CLIENT_ID": c.CRMCreditClientID, "CRM_CREDIT_CLIENT_SECRET": c.CRMCreditClientSecret} {
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

// LoadDatabase 有意保持窄范围，使迁移任务无需
// 提供其从不使用的浏览器或 OIDC 密钥。
func LoadDatabase() (string, error) {
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_MYSQL_DSN"))
	if dsn == "" {
		return "", fmt.Errorf("SETTLEMENT_MYSQL_DSN is required")
	}
	return dsn, nil
}

// isLoopbackHTTPOrigin 判断 http/https origin 是否指向本机（localhost/127.0.0.1/::1），
// 本机明文与自签调试不视为生产特征；非本机入口一律按公网生产入口处理。
func isLoopbackHTTPOrigin(lowerOrigin string) bool {
	host := lowerOrigin
	if idx := strings.Index(host, "://"); idx >= 0 {
		host = host[idx+3:]
	}
	if idx := strings.IndexAny(host, "/?#"); idx >= 0 {
		host = host[:idx]
	}
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "::1" || host == "127.0.0.1" || strings.HasPrefix(host, "127.")
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

// parseTrustedProxies 解析逗号分隔的可信代理地址或 CIDR；非法项直接报错，
// 避免拼写错误被静默忽略后仍以"谁都不信任/全信任"运行。
func parseTrustedProxies(value string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0)
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(item); err == nil {
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		address, err := netip.ParseAddr(item)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
	}
	return prefixes, nil
}
