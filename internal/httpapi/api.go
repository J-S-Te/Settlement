package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/handover"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
)

type API struct {
	handover        *handover.Service
	handoverMachine handoverVerifier
	service         *service.Service
	cfg             config.Config
	logger          *slog.Logger
	auth            *platform.Authenticator
	machine         *platform.ServiceTokenVerifier
	taxMachine      *platform.ServiceTokenVerifier
	directory       platform.PersonnelDirectory
	files           InvoiceFileGateway
	commercial      CommercialLicenseGate
}

type InvoiceFileGateway interface {
	service.InvoiceArchiveGateway
	Download(context.Context, string) ([]byte, error)
}

// New 用途：按结算权限、责任范围和错误契约处理请求。
// 参数：包含当前请求、可信主体或显式服务依赖；不接受客户端声明扩大权限。
// 返回值：返回处理器、主体或HTTP结果，错误保持失败关闭。
func New(db *sql.DB, cfg config.Config, logger *slog.Logger, auth *platform.Authenticator, machine, taxMachine *platform.ServiceTokenVerifier, dependencies ...any) http.Handler {
	var erp service.ERPProvider
	var projectReferences service.ProjectReferenceResolver
	var directory platform.PersonnelDirectory
	var publisher service.CreditEventPublisher
	var files InvoiceFileGateway
	var commercial CommercialLicenseGate
	var handoverService *handover.Service
	var handoverMachine handoverVerifier
	for _, dependency := range dependencies {
		if value, ok := dependency.(service.ERPProvider); ok {
			erp = value
		}
		if value, ok := dependency.(service.ProjectReferenceResolver); ok {
			projectReferences = value
		}
		if value, ok := dependency.(*handover.Service); ok {
			handoverService = value
		}
		if value, ok := dependency.(handoverVerifier); ok {
			handoverMachine = value
		}
		if value, ok := dependency.(CommercialLicenseGate); ok {
			commercial = value
		}
		if value, ok := dependency.(platform.PersonnelDirectory); ok {
			directory = value
		}
		if value, ok := dependency.(service.CreditEventPublisher); ok {
			publisher = value
		}
		if value, ok := dependency.(InvoiceFileGateway); ok {
			files = value
		}
	}
	api := &API{service: &service.Service{DB: db, CreditPublisher: publisher, InvoiceIssuanceMode: cfg.InvoiceIssuanceMode, TaxProviderCode: cfg.TaxProviderCode}, cfg: cfg, logger: logger, auth: auth, machine: machine, taxMachine: taxMachine, directory: directory, files: files}
	api.service.ERPProvider = erp
	api.service.ProjectReferences = projectReferences
	if roles, ok := directory.(service.RoleDirectory); ok {
		api.service.RoleDirectory = roles
	}
	api.commercial = commercial
	api.service.CheckInvoiceArchiveLicense = api.checkInvoiceArchiveLicense
	if handoverService != nil && handoverMachine != nil {
		api.handover, api.handoverMachine = handoverService, handoverMachine
	}
	if cfg.DevelopmentAuth && logger != nil {
		// 商业授权不替代既有身份及 CSRF 边界。
		// 响亮警告（SEC-D10）：DevelopmentAuth 让所有结算写接口免鉴权（回退
		// dev-finance 全权限主体）。它只允许出现在本机调试；与生产特征的组合
		// 已在 config.Load 启动期拒绝，这里保证任何启用时刻都有醒目日志可告警。
		logger.Error("!!! settlement DEVELOPMENT AUTH ENABLED: every write API is unauthenticated (dev-finance principal) - SETTLEMENT_DEVELOPMENT_AUTH MUST BE false OUTSIDE LOCAL DEVELOPMENT !!!",
			"development_auth", true,
			"public_origin", cfg.PublicOrigin,
			"environment_code", cfg.OIDCEnvironmentCode,
		)
	}
	return api
}

// csrfWriteAllowed 判定 /api/v1 写请求的来源是否可信（cookie 会话 CSRF 防线，SEC-D11）。
//
// 安全理由：前端 X-CSRF-Token 是常量 "1"，没有会话熵，防护完全落在来源校验上：
//   - PublicOrigin 为空（本地误配置/模板缺键）必须失败关闭——原来的字符串相等
//     比较会让“缺失的 Origin 头”与空基准相等，从而放行任何跨站写请求；
//   - Origin 必须存在且与 PublicOrigin 完全一致（缺失/跨源一律拒绝）；
//   - Sec-Fetch-Site: cross-site 是浏览器元数据头，直接拒绝（纵深防御，
//     它不能用于放行缺失 Origin 的请求）。
//
// 机器调用走 /internal/v1 Bearer 边界，不经过本校验。
func (a *API) csrfWriteAllowed(r *http.Request) bool {
	if r.Header.Get("X-CSRF-Token") != "1" {
		return false
	}
	expected := strings.TrimRight(strings.TrimSpace(a.cfg.PublicOrigin), "/")
	if expected == "" {
		// 失败关闭：没有可信来源基准时宁可拒绝全部写请求，也不放行。
		return false
	}
	origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/")
	if origin == "" || !strings.EqualFold(origin, expected) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	return true
}

// ServeHTTP 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if requestID == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		requestID = hex.EncodeToString(b)
	}
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if a.commercial != nil && a.commercial.Check(r.Context(), commercialOperation(r.Method, r.URL.Path)) != nil {
		fail(w, http.StatusForbidden, "COMMERCIAL_LICENSE_DENIED", "商业授权不可用或已到期；历史查询与导出仍受原权限控制")
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !a.csrfWriteAllowed(r) {
			fail(w, http.StatusForbidden, "SETTLEMENT_CSRF_REJECTED", "写请求来源校验失败")
			return
		}
	}
	if a.serveRoleFinance(w, r) || a.serveRoleCollaboration(w, r) {
		return
	}
	switch {
	case r.URL.Path == "/healthz":
		write(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	case r.URL.Path == "/readyz":
		if err := a.service.DB.PingContext(r.Context()); err != nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_NOT_READY", "数据库不可用")
			return
		}
		write(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	case r.URL.Path == "/auth/login":
		a.login(w, r)
		return
	case r.URL.Path == "/auth/callback":
		if a.auth == nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_OIDC_UNCONFIGURED", "Settlement 身份服务尚未配置")
		} else {
			a.auth.Callback(w, r)
		}
		return
	case r.URL.Path == "/auth/logout":
		if a.auth == nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_OIDC_UNCONFIGURED", "Settlement 身份服务尚未配置")
		} else {
			a.auth.Logout(w, r)
		}
		return
	case r.URL.Path == "/auth/local-logout" && r.Method == http.MethodPost:
		if a.auth == nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_OIDC_UNCONFIGURED", "Settlement 身份服务尚未配置")
		} else {
			a.auth.LogoutLocal(w, r)
		}
		return
	case r.URL.Path == "/auth/backchannel-logout" && r.Method == http.MethodPost:
		if a.auth == nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_OIDC_UNCONFIGURED", "Settlement 身份服务尚未配置")
		} else {
			a.auth.BackchannelLogout(w, r)
		}
		return
	case r.URL.Path == "/auth/me" || r.URL.Path == "/api/v1/auth/me":
		a.me(w, r)
		return
	case r.URL.Path == "/logged-out":
		write(w, http.StatusOK, map[string]string{"status": "logged_out"})
		return
	case r.URL.Path == "/internal/v1/settlement/events/contracts" && r.Method == http.MethodPost:
		a.contractEvent(w, r)
		return
	case r.URL.Path == "/internal/v1/settlement/events/tax-results" && r.Method == http.MethodPost:
		a.taxResult(w, r)
		return
	case a.handoverBound() && r.URL.Path == "/internal/v1/handover/freeze" && r.Method == http.MethodPost:
		a.handoverFreeze(w, r)
		return
	case a.handoverBound() && r.URL.Path == "/internal/v1/handover/responsibilities" && r.Method == http.MethodGet:
		a.handoverResponsibilities(w, r)
		return
	case a.handoverBound() && r.URL.Path == "/internal/v1/handover/transfers" && r.Method == http.MethodPost:
		a.handoverTransfer(w, r)
		return
	case a.handoverBound() && strings.HasPrefix(r.URL.Path, "/internal/v1/handover/transfers/") && r.Method == http.MethodGet:
		a.handoverReceipt(w, r)
		return
	case a.handoverBound() && r.URL.Path == "/internal/v1/handover/complete" && r.Method == http.MethodPost:
		a.handoverComplete(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/credit-sync-events/") && strings.HasSuffix(r.URL.Path, "/retry") && r.Method == http.MethodPost:
		a.retryCreditSync(w, r)
		return
	case r.URL.Path == "/api/v1/dashboard" && r.Method == http.MethodGet:
		a.dashboard(w, r)
		return
	case r.URL.Path == "/api/v1/reports/invoiced-receivables-top10" && r.Method == http.MethodGet:
		a.invoicedReceivablesTop10(w, r)
		return
	case r.URL.Path == "/api/v1/reports/invoiced-receivables/export" && r.Method == http.MethodPost:
		a.createInvoicedReceivablesExport(w, r)
		return
	case r.URL.Path == "/api/v1/outbox/events" && r.Method == http.MethodGet:
		a.listOutboxEvents(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/outbox/events/") && strings.HasSuffix(r.URL.Path, "/retry") && r.Method == http.MethodPost:
		a.retryOutboxEvent(w, r)
		return
	case r.URL.Path == "/api/v1/reports/aging" && r.Method == http.MethodGet:
		a.agingSummary(w, r)
		return
	case r.URL.Path == "/api/v1/reports/aging/export" && r.Method == http.MethodPost:
		a.createAgingReceivablesExport(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/reports/exports/") && strings.HasSuffix(r.URL.Path, "/download") && r.Method == http.MethodGet:
		a.downloadReportExport(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/reports/exports/") && r.Method == http.MethodGet:
		a.reportExport(w, r)
		return
	case r.URL.Path == "/api/v1/receivable-plans" && r.Method == http.MethodGet:
		a.listPlans(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/receivable-plans/") && strings.HasSuffix(r.URL.Path, "/confirm") && r.Method == http.MethodPost:
		a.confirmPlan(w, r)
		return
	case r.URL.Path == "/api/v1/receivables" && r.Method == http.MethodGet:
		a.listReceivables(w, r, false)
		return
	case r.URL.Path == "/api/v1/invoice-eligible-receivables" && r.Method == http.MethodGet:
		// 开票选择器只读取结算库快照，避免浏览器直接访问合同管理数据库。
		a.listReceivables(w, r, true)
		return
	case r.URL.Path == "/api/v1/receipts" && r.Method == http.MethodGet:
		a.listReceipts(w, r)
		return
	case r.URL.Path == "/api/v1/receipts" && r.Method == http.MethodPost:
		a.createReceipt(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/receipts/") && strings.HasSuffix(r.URL.Path, "/matches") && r.Method == http.MethodGet:
		a.receiptMatches(w, r)
		return
	case r.URL.Path == "/api/v1/receipt-allocations" && r.Method == http.MethodPost:
		a.createAllocation(w, r)
		return
	case r.URL.Path == "/api/v1/receipt-allocations" && r.Method == http.MethodGet:
		a.listAllocations(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/receipt-allocations/") && strings.HasSuffix(r.URL.Path, "/reverse") && r.Method == http.MethodPost:
		a.reverseAllocation(w, r)
		return
	case r.URL.Path == "/api/v1/invoice-requests" && r.Method == http.MethodGet:
		a.listInvoiceRequests(w, r)
		return
	case r.URL.Path == "/api/v1/tax-invoices" && r.Method == http.MethodGet:
		a.listTaxInvoices(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/tax-invoices/") && strings.HasSuffix(r.URL.Path, "/documents") && r.Method == http.MethodPost:
		a.uploadInvoiceDocument(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/tax-invoices/") && strings.HasSuffix(r.URL.Path, "/download") && r.Method == http.MethodGet:
		a.downloadInvoiceDocument(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/tax-invoices/") && strings.HasSuffix(r.URL.Path, "/red-flush") && r.Method == http.MethodPost:
		a.requestInvoiceRedFlush(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-red-flush-requests/") && strings.HasSuffix(r.URL.Path, "/approve") && r.Method == http.MethodPost:
		a.reviewInvoiceRedFlush(w, r, true)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-red-flush-requests/") && strings.HasSuffix(r.URL.Path, "/reject") && r.Method == http.MethodPost:
		a.reviewInvoiceRedFlush(w, r, false)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/tax-invoices/") && r.Method == http.MethodGet:
		a.taxInvoiceDetail(w, r)
		return
	case r.URL.Path == "/api/v1/invoice-requests" && r.Method == http.MethodPost:
		a.createInvoiceRequest(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-requests/") && strings.HasSuffix(r.URL.Path, "/approve") && r.Method == http.MethodPost:
		a.approveInvoiceRequest(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-requests/") && strings.HasSuffix(r.URL.Path, "/recover-failure") && r.Method == http.MethodPost:
		a.recoverInvoiceFailure(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-requests/") && strings.HasSuffix(r.URL.Path, "/reject") && r.Method == http.MethodPost:
		a.rejectInvoiceRequest(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-requests/") && strings.HasSuffix(r.URL.Path, "/manual-issue") && r.Method == http.MethodPost:
		a.manualIssueInvoice(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/policies" && r.Method == http.MethodGet:
		a.listDunningPolicies(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/policies" && r.Method == http.MethodPost:
		a.createDunningPolicy(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/overview" && r.Method == http.MethodGet:
		a.dunningOverview(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/coverage" && r.Method == http.MethodGet:
		a.dunningCoverage(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/cases" && r.Method == http.MethodGet:
		a.listDunningCases(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/actions" && r.Method == http.MethodGet:
		a.listDunningActions(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/dunning/actions/") && strings.HasSuffix(r.URL.Path, "/complete") && r.Method == http.MethodPost:
		a.completeDunningAction(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/recipients" && r.Method == http.MethodGet:
		a.listDunningRecipients(w, r)
		return
	case r.URL.Path == "/api/v1/notifications" && r.Method == http.MethodGet:
		a.listNotifications(w, r)
		return
	case r.URL.Path == "/api/v1/notifications/unread-count" && r.Method == http.MethodGet:
		a.unreadCount(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/notifications/") && strings.HasSuffix(r.URL.Path, "/read") && r.Method == http.MethodPost:
		a.readNotification(w, r)
		return
	default:
		fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到接口")
	}
}

func (a *API) retryCreditSync(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.credit.resend")
	if !ok {
		return
	}
	eventID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/credit-sync-events/"), "/retry")
	if eventID == "" || strings.Contains(eventID, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "信用同步事件标识不合法")
		return
	}
	if err := a.service.RetryCreditSync(r.Context(), p.TenantID, eventID); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到信用同步事件")
			return
		}
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_CREDIT_SYNC_FAILED", "CRM 信用同步失败，已保留待重发状态")
		return
	}
	write(w, http.StatusOK, map[string]string{"event_id": eventID, "status": "DELIVERED"})
}

// 开发认证必须显式且仅限本地。生产在暴露任何业务
// 路由前，必须安装与其他子系统相同的 OIDC 会话
// 适配器及客户端凭据校验器。
// clientIP 解析本次业务操作的客户端地址与来源标记。
//
// 只有直接对端命中配置的可信代理时才采信 X-Real-IP / X-Forwarded-For；XFF 从右往左
// 跳过可信代理本身，取第一个非代理地址。私网、VPN、回环都是合法客户端地址。
func (a *API) clientIP(r *http.Request) (string, string) {
	peer := normalizePeerAddress(r.RemoteAddr)
	if peer != "" && a.trustsProxy(peer) {
		if ip := normalizeIPAddress(r.Header.Get("X-Real-IP")); ip != "" {
			return ip, "TRUSTED_PROXY"
		}
		values := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for i := len(values) - 1; i >= 0; i-- {
			ip := normalizeIPAddress(values[i])
			if ip == "" || a.trustsProxy(ip) {
				continue
			}
			return ip, "TRUSTED_PROXY"
		}
	}
	if peer != "" {
		return peer, "DIRECT"
	}
	return "", "UNKNOWN"
}

func (a *API) trustsProxy(value string) bool {
	address, err := netip.ParseAddr(value)
	if err != nil {
		return false
	}
	for _, prefix := range a.cfg.TrustedProxies {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func normalizePeerAddress(remoteAddr string) string {
	value := strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	return normalizeIPAddress(strings.Trim(value, "[]"))
}

func normalizeIPAddress(value string) string {
	parsed, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || parsed.IsUnspecified() || parsed.IsMulticast() {
		return ""
	}
	return parsed.String()
}

func (a *API) principal(r *http.Request) (service.Principal, error) {
	if !a.cfg.DevelopmentAuth {
		if a.auth == nil {
			return service.Principal{}, errors.New("OIDC adapter is not configured")
		}
		principal, err := a.auth.Authenticate(r.Context(), r)
		if err != nil {
			return principal, err
		}
		// AUD-IP-2026：登录地址来自平台授权上下文（随会话固定），操作地址按请求解析，
		// 由业务事务写入审计 Outbox。handler 与 principal 共享同一 *http.Request，
		// 就地替换上下文后，后续 service.X(r.Context(), …) 能读到来源元数据。
		clientIP, ipSource := a.clientIP(r)
		*r = *r.WithContext(service.WithRequestMeta(r.Context(), service.RequestMeta{
			LoginIP: principal.LoginIP, ClientIP: clientIP, IPSource: ipSource,
		}))
		return principal, nil
	}
	return service.Principal{TenantID: "dev", UserID: "dev-finance", IdentityID: "dev-finance", DisplayName: "本地结算管理员", Username: "dev-finance", CatalogVersion: "development", AuthorizationRevision: 1, Permissions: map[string]bool{"settlement.admin": true}}, nil
}
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DevelopmentAuth {
		http.Redirect(w, r, "/settlement/dashboard", http.StatusFound)
		return
	}
	if a.auth == nil {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_OIDC_UNCONFIGURED", "Settlement OIDC 会话适配器尚未配置")
		return
	}
	a.auth.Login(w, r)
}

// me 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) me(w http.ResponseWriter, r *http.Request) {
	p, err := a.principal(r)
	if err != nil {
		if errors.Is(err, platform.ErrUnauthenticated) {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_UNAUTHENTICATED", "登录已失效，请重新登录")
		} else {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_IDENTITY_UNAVAILABLE", "身份服务暂不可用")
		}
		return
	}
	permissions := make([]string, 0, len(p.Permissions))
	for permission, allowed := range p.Permissions {
		if allowed {
			permissions = append(permissions, permission)
		}
	}
	write(w, http.StatusOK, map[string]any{
		"tenant_id": p.TenantID, "user_id": p.UserID, "identity_id": p.IdentityID,
		"person_id": p.PersonID, "name": p.DisplayName, "preferred_username": p.Username,
		"permissions": permissions, "catalog_version": p.CatalogVersion,
		"roles": p.Roles, "data_scopes": p.DataScopes,
		"authorization_revision": p.AuthorizationRevision,
	})
}

// integration 验证机器令牌并返回其绑定的可信租户。生产模式下来自验签身份
// （identity.TenantID，与验签器配置租户一致）；DevelopmentAuth 下没有可验签
// 身份，回退到配置租户（OIDC_TENANT_ID）。返回的可信租户可能为空，调用方
// 必须对空租户失败关闭（AUD-2026-001），不得回退到报文体声明的租户。
func (a *API) integration(r *http.Request) (string, bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !a.cfg.IntegrationEnabled || token == "" {
		return "", false
	}
	if a.cfg.DevelopmentAuth {
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.IntegrationBearerToken)) != 1 {
			return "", false
		}
		return a.cfg.OIDCTenantID, true
	}
	if a.machine == nil {
		return "", false
	}
	identity, err := a.machine.Verify(r.Context(), token)
	if err != nil {
		return "", false
	}
	return identity.TenantID, true
}
func (a *API) contractEvent(w http.ResponseWriter, r *http.Request) {
	trustedTenant, ok := a.integration(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "SETTLEMENT_MACHINE_UNAUTHENTICATED", "服务间令牌无效")
		return
	}
	// AUD-2026-001：可信租户缺失时失败关闭，绝不采信请求体声明的租户。
	if strings.TrimSpace(trustedTenant) == "" {
		fail(w, http.StatusForbidden, "SETTLEMENT_MACHINE_TENANT_UNRESOLVED", "服务间令牌未绑定可信租户")
		return
	}
	var event service.ContractEvent
	if !decode(w, r, &event) {
		return
	}
	created, err := a.service.IngestContract(r.Context(), "contract_management", trustedTenant, event)
	if err != nil {
		if a.logger != nil {
			a.logger.Error("contract financial event ingest failed", "event_id", event.EventID, "contract_id", event.Contract.ID, "error", err)
		}
		respondError(w, err)
		return
	}
	write(w, http.StatusAccepted, map[string]any{"event_id": event.EventID, "accepted": true, "duplicate": !created})
}
func (a *API) taxResult(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !a.cfg.TaxResultIngestEnabled || token == "" {
		fail(w, http.StatusUnauthorized, "SETTLEMENT_TAX_MACHINE_UNAUTHENTICATED", "税控结果服务令牌无效")
		return
	}
	trustedTenant := a.cfg.OIDCTenantID
	if a.cfg.DevelopmentAuth {
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.TaxResultBearerToken)) != 1 {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_TAX_MACHINE_UNAUTHENTICATED", "税控结果服务令牌无效")
			return
		}
	} else {
		if a.taxMachine == nil {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_TAX_MACHINE_UNAUTHENTICATED", "税控结果服务令牌无效")
			return
		}
		identity, err := a.taxMachine.Verify(r.Context(), token)
		if err != nil {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_TAX_MACHINE_UNAUTHENTICATED", "税控结果服务令牌无效")
			return
		}
		trustedTenant = identity.TenantID
	}
	// AUD-2026-020：税控租户必须来自验签身份或配置；缺失即失败关闭，
	// 绝不回退到报文体声明的租户。
	if strings.TrimSpace(trustedTenant) == "" {
		fail(w, http.StatusForbidden, "SETTLEMENT_TAX_TENANT_UNRESOLVED", "税控回调租户未从验签身份或配置解析")
		return
	}
	var event service.TaxCallbackEvent
	if !decode(w, r, &event) {
		return
	}
	result, err := a.service.ApplyTaxCallback(r.Context(), trustedTenant, event)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusAccepted, map[string]any{"event_id": event.EventID, "accepted": true, "processing_status": result, "duplicate": result != "APPLIED"})
}
func (a *API) dashboard(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.report.read")
	if !ok {
		return
	}
	var receivables, overdue, plans, invoices int
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*),COALESCE(SUM(CASE WHEN due_date<CURDATE() AND open_amount>0 THEN 1 ELSE 0 END),0) FROM settlement_receivable WHERE tenant_id=?`, p.TenantID).Scan(&receivables, &overdue); err != nil {
		respondError(w, err)
		return
	}
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM settlement_receivable_plan WHERE tenant_id=? AND confirmation_status='PENDING_CONFIRMATION'`, p.TenantID).Scan(&plans); err != nil {
		respondError(w, err)
		return
	}
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM settlement_invoice_request WHERE tenant_id=? AND status IN ('SUBMITTED','UNDER_REVIEW','ISSUE_PENDING')`, p.TenantID).Scan(&invoices); err != nil {
		respondError(w, err)
		return
	}
	// 币种合计在 SQL 中覆盖整个租户计算。看板
	// 绝不能累加不同币种金额，也不能依赖列表请求的
	// 第一页。
	type totals struct{ open, receipts, pendingInvoice, overdue string }
	byCurrency := map[string]*totals{}
	get := func(currency string) *totals {
		currency = strings.ToUpper(strings.TrimSpace(currency))
		if currency == "" {
			currency = "CNY"
		}
		if byCurrency[currency] == nil {
			byCurrency[currency] = &totals{open: "0.00", receipts: "0.00", pendingInvoice: "0.00", overdue: "0.00"}
		}
		return byCurrency[currency]
	}
	addRows := func(query, field string) error {
		rows, err := a.service.DB.QueryContext(r.Context(), query, p.TenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var currency, value string
			if err := rows.Scan(&currency, &value); err != nil {
				return err
			}
			t := get(currency)
			switch field {
			case "open":
				t.open = value
			case "receipts":
				t.receipts = value
			case "pending":
				t.pendingInvoice = value
			case "overdue":
				t.overdue = value
			}
		}
		return rows.Err()
	}
	if err := addRows(`SELECT currency,COALESCE(SUM(open_amount),0) FROM settlement_receivable WHERE tenant_id=? AND open_amount>0 GROUP BY currency ORDER BY currency`, "open"); err != nil {
		respondError(w, err)
		return
	}
	if err := addRows(`SELECT currency,COALESCE(SUM(amount),0) FROM settlement_receipt WHERE tenant_id=? AND receipt_date>=DATE_FORMAT(CURDATE(),'%Y-%m-01') AND receipt_date<DATE_ADD(DATE_FORMAT(CURDATE(),'%Y-%m-01'),INTERVAL 1 MONTH) GROUP BY currency ORDER BY currency`, "receipts"); err != nil {
		respondError(w, err)
		return
	}
	if err := addRows(`SELECT cs.currency,COALESCE(SUM(ir.amount_incl_tax),0) FROM settlement_invoice_request ir JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id WHERE ir.tenant_id=? AND ir.status IN ('SUBMITTED','UNDER_REVIEW','ISSUE_PENDING') GROUP BY cs.currency ORDER BY cs.currency`, "pending"); err != nil {
		respondError(w, err)
		return
	}
	if err := addRows(`SELECT currency,COALESCE(SUM(open_amount),0) FROM settlement_receivable WHERE tenant_id=? AND open_amount>0 AND due_date<CURDATE() GROUP BY currency ORDER BY currency`, "overdue"); err != nil {
		respondError(w, err)
		return
	}
	currencyTotals := make([]map[string]string, 0, len(byCurrency))
	for currency, t := range byCurrency {
		currencyTotals = append(currencyTotals, map[string]string{
			"currency": currency, "open_receivable": t.open, "current_month_receipts": t.receipts,
			"pending_invoice": t.pendingInvoice, "overdue": t.overdue,
		})
	}
	sort.Slice(currencyTotals, func(i, j int) bool { return currencyTotals[i]["currency"] < currencyTotals[j]["currency"] })
	write(w, http.StatusOK, map[string]any{"receivable_count": receivables, "overdue_count": overdue, "pending_plan_count": plans, "pending_invoice_count": invoices, "currency_totals": currencyTotals})
}
func (a *API) listOutboxEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.admin")
	if !ok {
		return
	}
	items, err := a.service.ListOutboxEvents(r.Context(), p)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) retryOutboxEvent(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.admin")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/outbox/events/"), "/retry")
	if err := a.service.RetryOutboxEvent(r.Context(), p, id); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]string{"id": id, "status": "PENDING"})
}

// agingSummary 用途：按结算权限、责任范围和错误契约处理请求。
// 参数：包含当前请求、可信主体或显式服务依赖；不接受客户端声明扩大权限。
// 返回值：返回处理器、主体或HTTP结果，错误保持失败关闭。
func (a *API) agingSummary(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.report.read")
	if !ok {
		return
	}
	items, err := a.service.AgingSummary(r.Context(), p)
	if err != nil {
		respondError(w, err)
		return
	}
	days, err := a.service.AgingDays(r.Context(), p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"items": items, "aging_days": days})
}

func (a *API) invoicedReceivablesTop10(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.report.read")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT r.id,r.receivable_no,cs.source_contract_no,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.open_amount,r.currency,r.collection_status,r.invoice_status FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id WHERE r.tenant_id=? AND r.recognition_status='CONFIRMED' AND r.open_amount>0 AND r.invoice_status<>'NOT_INVOICED' ORDER BY r.currency ASC,r.open_amount DESC,r.due_date ASC,r.id ASC LIMIT 10`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, contract, customer, due, open, currency, collection, invoice string
		if err := rows.Scan(&id, &no, &contract, &customer, &due, &open, &currency, &collection, &invoice); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"receivable_id": id, "receivable_no": no, "contract_no": contract, "customer_name": customer, "due_date": due, "open_amount": open, "currency": currency, "collection_status": collection, "invoice_status": invoice})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"as_of_date": time.Now().UTC().Format("2006-01-02"), "items": items})
}
func (a *API) createInvoicedReceivablesExport(w http.ResponseWriter, r *http.Request) {
	a.createReportExport(w, r, "INVOICED_RECEIVABLES_CSV")
}
func (a *API) createAgingReceivablesExport(w http.ResponseWriter, r *http.Request) {
	a.createReportExport(w, r, "AGING_RECEIVABLES_CSV")
}
func (a *API) createReportExport(w http.ResponseWriter, r *http.Request, reportType string) {
	p, ok := a.user(w, r, "settlement.report.export")
	if !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "需要有效的 Idempotency-Key")
		return
	}
	var id, existingType string
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT id,report_type FROM settlement_report_export_job WHERE tenant_id=? AND requested_by=? AND idempotency_key=?`, p.TenantID, p.UserID, key).Scan(&id, &existingType)
	if err == nil && existingType != reportType {
		fail(w, http.StatusConflict, "SETTLEMENT_IDEMPOTENCY_CONFLICT", "幂等键已用于其他导出任务")
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		id = newRequestID()
		_, err = a.service.DB.ExecContext(r.Context(), `INSERT INTO settlement_report_export_job(id,tenant_id,requested_by,report_type,idempotency_key,status,created_at,updated_at) VALUES(?,?,?,?,?,'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, p.UserID, reportType, key)
	}
	if err != nil {
		respondError(w, err)
		return
	}
	a.writeReportExport(w, r, p, id, http.StatusAccepted)
}
func (a *API) reportExport(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.report.export")
	if !ok {
		return
	}
	a.writeReportExport(w, r, p, strings.TrimPrefix(r.URL.Path, "/api/v1/reports/exports/"), http.StatusOK)
}
func (a *API) writeReportExport(w http.ResponseWriter, r *http.Request, p service.Principal, id string, statusCode int) {
	var reportType, status, fileName, errorMessage string
	var createdAt, completedAt, expiresAt sql.NullTime
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT report_type,status,file_name,error_message,created_at,completed_at,expires_at FROM settlement_report_export_job WHERE id=? AND tenant_id=? AND requested_by=?`, id, p.TenantID, p.UserID).Scan(&reportType, &status, &fileName, &errorMessage, &createdAt, &completedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到导出任务")
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	item := map[string]any{"id": id, "report_type": reportType, "status": status, "file_name": fileName, "error_message": errorMessage, "created_at": createdAt.Time.Format(time.RFC3339), "completed_at": "", "expires_at": ""}
	if completedAt.Valid {
		item["completed_at"] = completedAt.Time.Format(time.RFC3339)
	}
	if expiresAt.Valid {
		item["expires_at"] = expiresAt.Time.Format(time.RFC3339)
	}
	write(w, statusCode, item)
}
func (a *API) downloadReportExport(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.report.export")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/reports/exports/"), "/download")
	var status, filename, contentType string
	var content []byte
	var expiresAt sql.NullTime
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT status,file_name,content_type,file_content,expires_at FROM settlement_report_export_job WHERE id=? AND tenant_id=? AND requested_by=?`, id, p.TenantID, p.UserID).Scan(&status, &filename, &contentType, &content, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到导出任务")
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	if expiresAt.Valid && expiresAt.Time.Before(time.Now().UTC()) {
		fail(w, http.StatusGone, "SETTLEMENT_EXPORT_EXPIRED", "导出文件已过期，请重新导出")
		return
	}
	if status != "READY" || len(content) == 0 {
		fail(w, http.StatusConflict, "SETTLEMENT_EXPORT_NOT_READY", "导出文件尚未生成完成")
		return
	}
	if err = a.service.AuditAction(r.Context(), p, "SETTLEMENT_REPORT_EXPORT_DOWNLOADED", "report_export", id, map[string]any{"report_type": "AGING_OR_INVOICED", "risk_level": "HIGH"}); err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(filename, `"`, "")+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}
func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

// listPlans 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) listPlans(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receivable.read")
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	rows, err := a.queryScoped(r.Context(), p, "settlement.receivable.read", `SELECT rp.id,cs.source_contract_no,cs.customer_name_snapshot,rp.installment_no,DATE_FORMAT(rp.due_date,'%Y-%m-%d'),rp.planned_amount,rp.confirmation_status,rp.version FROM settlement_receivable_plan rp JOIN settlement_contract_snapshot cs ON cs.id=rp.contract_snapshot_id AND cs.tenant_id=rp.tenant_id WHERE rp.tenant_id=? ORDER BY rp.due_date ASC,rp.id ASC LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, customer, due, planned, status string
		var installment, version int
		if err := rows.Scan(&id, &no, &customer, &installment, &due, &planned, &status, &version); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "contract_no": no, "customer_name": customer, "installment_no": installment, "due_date": due, "planned_amount": planned, "status": status, "version": version})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

// confirmPlan 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) confirmPlan(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receivable.confirm")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/receivable-plans/"), "/confirm")
	if !a.recordAllowed(w, r, p, "settlement.receivable.confirm", "plan", id) {
		return
	}
	var body service.PlanConfirmationInput
	if !decode(w, r, &body) {
		return
	}
	if err := a.service.ConfirmPlan(r.Context(), p, id, body); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

func (a *API) receiptMatches(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.allocation.confirm")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/receipts/"), "/matches")
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT r.id,r.receivable_no,cs.source_contract_no,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.open_amount,r.currency,LEAST(r.open_amount,rc.unallocated_amount),CASE WHEN r.open_amount=rc.unallocated_amount THEN 100 WHEN r.open_amount>rc.unallocated_amount THEN 80 ELSE 70 END FROM settlement_receipt rc JOIN settlement_receivable r ON r.tenant_id=rc.tenant_id AND r.currency=rc.currency JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id AND cs.customer_id=rc.customer_id WHERE rc.id=? AND rc.tenant_id=? AND rc.unallocated_amount>0 AND r.recognition_status='CONFIRMED' AND r.open_amount>0 ORDER BY 9 DESC,r.due_date ASC,r.id ASC LIMIT 20`, id, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var receivableID, no, contract, customer, due, open, currency, suggested string
		var score int
		if err := rows.Scan(&receivableID, &no, &contract, &customer, &due, &open, &currency, &suggested, &score); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"receivable_id": receivableID, "receivable_no": no, "contract_no": contract, "customer_name": customer, "due_date": due, "open_amount": open, "currency": currency, "suggested_amount": suggested, "score": score})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

// listReceivables 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) listReceivables(w http.ResponseWriter, r *http.Request, invoiceEligibleOnly bool) {
	permission := "settlement.receivable.read"
	if invoiceEligibleOnly {
		permission = "settlement.invoice.request"
	}
	p, ok := a.user(w, r, permission)
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	where := []string{"r.tenant_id = ?"}
	args := []any{p.TenantID}
	if invoiceEligibleOnly {
		where = append(where, "cs.financial_status = 'EFFECTIVE'", "r.recognition_status = 'CONFIRMED'", "r.original_amount-r.invoiced_amount-COALESCE((SELECT SUM(a.reserved_amount-a.invoiced_amount-a.released_amount) FROM settlement_invoice_request_allocation a WHERE a.tenant_id=r.tenant_id AND a.receivable_id=r.id AND a.status='RESERVED'),0) > 0")
	}
	if keyword != "" {
		where = append(where, "(r.receivable_no LIKE ? OR cs.source_contract_no LIKE ? OR cs.customer_name_snapshot LIKE ?)")
		like := "%" + keyword + "%"
		args = append(args, like, like, like)
	}
	query := `SELECT r.id,r.receivable_no,r.contract_snapshot_id,cs.source_contract_no,cs.customer_id,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.original_amount,r.open_amount,r.currency,r.collection_status,r.invoice_status,r.version, r.original_amount-r.invoiced_amount-COALESCE((SELECT SUM(a.reserved_amount-a.invoiced_amount-a.released_amount) FROM settlement_invoice_request_allocation a WHERE a.tenant_id=r.tenant_id AND a.receivable_id=r.id AND a.status='RESERVED'),0) FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id WHERE ` + strings.Join(where, " AND ") + ` ORDER BY r.due_date ASC,r.id ASC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := a.queryScoped(r.Context(), p, permission, query, args...)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, snapshot, contract, customerID, customer, due, original, open, currency, collection, invoice, invoiceable string
		var version int
		if err := rows.Scan(&id, &no, &snapshot, &contract, &customerID, &customer, &due, &original, &open, &currency, &collection, &invoice, &version, &invoiceable); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "receivable_no": no, "contract_snapshot_id": snapshot, "contract_no": contract, "customer_id": customerID, "customer_name": customer, "due_date": due, "original_amount": original, "open_amount": open, "invoiceable_amount": invoiceable, "currency": currency, "collection_status": collection, "invoice_status": invoice, "version": version})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

// listReceipts 用途：按结算权限、责任范围和错误契约处理请求。
// 参数：包含当前请求、可信主体或显式服务依赖；不接受客户端声明扩大权限。
// 返回值：返回处理器、主体或HTTP结果，错误保持失败关闭。
func (a *API) listReceipts(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receipt.read")
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT id,receipt_no,customer_name_snapshot,amount,unallocated_amount,currency,DATE_FORMAT(receipt_date,'%Y-%m-%d'),payment_method,bank_transaction_reference,status FROM settlement_receipt WHERE tenant_id=? ORDER BY receipt_date DESC,id DESC LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, customer, amount, unallocated, currency, date, method, reference, status string
		if err := rows.Scan(&id, &no, &customer, &amount, &unallocated, &currency, &date, &method, &reference, &status); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "receipt_no": no, "customer_name": customer, "amount": amount, "unallocated_amount": unallocated, "currency": currency, "receipt_date": date, "payment_method": method, "bank_transaction_reference": reference, "status": status})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

// page 解析有界列表分页，防止客户端请求无限
// 财务结果集。默认值保持与第一阶段 UI 兼容。
func page(r *http.Request) (int, int, error) {
	limit := 100
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return 0, 0, fmt.Errorf("limit 必须是 1 到 100 之间的整数")
		}
		limit = value
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > 100000 {
			return 0, 0, fmt.Errorf("offset 必须是 0 到 100000 之间的整数")
		}
		offset = value
	}
	return limit, offset, nil
}

func (a *API) createReceipt(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receipt.record")
	if !ok {
		return
	}
	var in service.ReceiptInput
	if !decode(w, r, &in) {
		return
	}
	id, err := a.service.CreateReceipt(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": id})
}
func (a *API) createAllocation(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.allocation.confirm")
	if !ok {
		return
	}
	var in service.AllocationInput
	if !decode(w, r, &in) {
		return
	}
	id, err := a.service.ConfirmAllocation(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": id, "status": "CONFIRMED"})
}

// listAllocations 用途：按结算权限、责任范围和错误契约处理请求。
// 参数：包含当前请求、可信主体或显式服务依赖；不接受客户端声明扩大权限。
// 返回值：返回处理器、主体或HTTP结果，错误保持失败关闭。
func (a *API) listAllocations(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.allocation.read")
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT a.id,a.allocation_no,rc.receipt_no,rv.receivable_no,a.allocated_amount,a.currency,a.status,a.match_mode,a.match_confidence,a.confirmed_by,a.confirmed_at FROM settlement_receipt_allocation a JOIN settlement_receipt rc ON rc.id=a.receipt_id AND rc.tenant_id=a.tenant_id JOIN settlement_receivable rv ON rv.id=a.receivable_id AND rv.tenant_id=a.tenant_id WHERE a.tenant_id=? ORDER BY a.confirmed_at DESC,a.id DESC LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, receipt, receivable, amount, currency, status, matchMode, by string
		var matchConfidence int
		var at any
		if err := rows.Scan(&id, &no, &receipt, &receivable, &amount, &currency, &status, &matchMode, &matchConfidence, &by, &at); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "allocation_no": no, "receipt_no": receipt, "receivable_no": receivable, "amount": amount, "currency": currency, "status": status, "match_mode": matchMode, "match_confidence": matchConfidence, "confirmed_by": by, "confirmed_at": at})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) reverseAllocation(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.allocation.reverse")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/receipt-allocations/"), "/reverse")
	var in service.ReversalInput
	if !decode(w, r, &in) {
		return
	}
	result, err := a.service.ReverseAllocation(r.Context(), p, r.Header.Get("Idempotency-Key"), id, in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": result, "status": "REVERSED"})
}

// createInvoiceRequest 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) createInvoiceRequest(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.request")
	if !ok {
		return
	}
	var in service.InvoiceRequestInput
	if !decode(w, r, &in) {
		return
	}
	if err := a.service.RequireContractAccess(r.Context(), p, "settlement.invoice.request", in.ContractSnapshotID); err != nil {
		respondError(w, err)
		return
	}
	id, err := a.service.CreateInvoiceRequest(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": id, "status": "SUBMITTED"})
}

// approveInvoiceRequest 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) approveInvoiceRequest(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.approve")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-requests/"), "/approve")
	var in struct {
		Version int `json:"version"`
	}
	if !decode(w, r, &in) {
		return
	}
	attempt, err := a.service.ApproveInvoiceRequest(r.Context(), p, id, in.Version)
	if err != nil {
		respondError(w, err)
		return
	}
	status := "ISSUE_PENDING"
	if attempt == "" {
		status = "SPECIAL_REVIEW"
	}
	write(w, http.StatusAccepted, map[string]string{"id": id, "attempt_id": attempt, "status": status})
}
func (a *API) rejectInvoiceRequest(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.approve")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-requests/"), "/reject")
	var in struct {
		Version int    `json:"version"`
		Reason  string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := a.service.RejectInvoiceRequest(r.Context(), p, id, in.Version, in.Reason); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]string{"id": id, "status": "REJECTED"})
}
func (a *API) manualIssueInvoice(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.issue")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-requests/"), "/manual-issue")
	var in service.ManualInvoiceInput
	if !decode(w, r, &in) {
		return
	}
	invoiceID, err := a.service.RegisterManualInvoice(r.Context(), p, id, in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": invoiceID, "status": "ISSUED"})
}

// listInvoiceRequests 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) listInvoiceRequests(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.read")
	if !ok {
		return
	}
	taxNumberVisible := true
	if p.CatalogVersion != "" {
		var policyErr error
		taxNumberVisible, policyErr = a.service.TaxNumberVisible(r.Context(), p.TenantID)
		if policyErr != nil {
			respondError(w, policyErr)
			return
		}
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	rows, err := a.queryScoped(r.Context(), p, "settlement.invoice.read", `SELECT ir.id,ir.request_no,cs.source_contract_no,ir.buyer_profile_snapshot,ir.invoice_type,ir.amount_excl_tax,ir.tax_amount,ir.amount_incl_tax,ir.status,ir.submitted_by,ir.approved_by,ir.rejection_reason,ir.version,ir.created_at,(SELECT COUNT(DISTINCT ii.tax_rate) FROM settlement_invoice_request_item ii WHERE ii.tenant_id=ir.tenant_id AND ii.invoice_request_id=ir.id),(SELECT MIN(ii.tax_rate) FROM settlement_invoice_request_item ii WHERE ii.tenant_id=ir.tenant_id AND ii.invoice_request_id=ir.id),COALESCE((SELECT ia.channel FROM settlement_invoice_issue_attempt ia WHERE ia.tenant_id=ir.tenant_id AND ia.invoice_request_id=ir.id ORDER BY ia.attempt_no DESC,ia.requested_at DESC LIMIT 1),''),COALESCE((SELECT ia.failure_code FROM settlement_invoice_issue_attempt ia WHERE ia.tenant_id=ir.tenant_id AND ia.invoice_request_id=ir.id ORDER BY ia.attempt_no DESC,ia.requested_at DESC LIMIT 1),''),COALESCE((SELECT ia.failure_message FROM settlement_invoice_issue_attempt ia WHERE ia.tenant_id=ir.tenant_id AND ia.invoice_request_id=ir.id ORDER BY ia.attempt_no DESC,ia.requested_at DESC LIMIT 1),''),COALESCE(sr.special_reason,''),COALESCE(sr.threshold_snapshot,''),COALESCE(sr.settings_version,0) FROM settlement_invoice_request ir LEFT JOIN settlement_invoice_special_review sr ON sr.invoice_request_id=ir.id AND sr.tenant_id=ir.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id WHERE ir.tenant_id=? ORDER BY ir.created_at DESC,ir.id DESC LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, contract, kind, excl, tax, incl, status, submitted, approved, rejectionReason, issueChannel, failureCode, failureMessage string
		var buyer []byte
		var taxRate sql.NullString
		var specialReason, thresholdSnapshot string
		var settingsVersion int
		var taxRateCount int
		var version int
		var created any
		if err := rows.Scan(&id, &no, &contract, &buyer, &kind, &excl, &tax, &incl, &status, &submitted, &approved, &rejectionReason, &version, &created, &taxRateCount, &taxRate, &issueChannel, &failureCode, &failureMessage, &specialReason, &thresholdSnapshot, &settingsVersion); err != nil {
			respondError(w, err)
			return
		}
		buyerName, buyerTaxNo := buyerSummary(buyer)
		if !taxNumberVisible {
			buyerTaxNo = ""
		}
		items = append(items, map[string]any{"id": id, "request_no": no, "contract_no": contract, "buyer_name": buyerName, "buyer_tax_no_masked": maskTaxNo(buyerTaxNo), "invoice_type": kind, "amount_excl_tax": excl, "tax_amount": tax, "amount_incl_tax": incl, "tax_rate_display": taxRateDisplay(taxRate.String, taxRateCount), "status": status, "submitted_by": submitted, "approved_by": approved, "rejection_reason": rejectionReason, "version": version, "created_at": created, "issue_channel": issueChannel, "failure_code": failureCode, "failure_message": failureMessage, "special_reason": specialReason, "threshold_snapshot": thresholdSnapshot, "settings_version": settingsVersion})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

// listTaxInvoices 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) listTaxInvoices(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.read")
	if !ok {
		return
	}
	taxNumberVisible := true
	if p.CatalogVersion != "" {
		var policyErr error
		taxNumberVisible, policyErr = a.service.TaxNumberVisible(r.Context(), p.TenantID)
		if policyErr != nil {
			respondError(w, policyErr)
			return
		}
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	rows, err := a.queryScoped(r.Context(), p, "settlement.invoice.read", `SELECT ti.id,ti.invoice_request_id,ti.invoice_code,ti.invoice_no,cs.source_contract_no,ti.buyer_profile_snapshot,ti.invoice_type,ti.amount_incl_tax,DATE_FORMAT(ti.issue_date,'%Y-%m-%d'),ti.status,ti.issued_by_channel,COALESCE((SELECT rf.status FROM settlement_invoice_red_flush_request rf WHERE rf.tenant_id=ti.tenant_id AND rf.original_invoice_id=ti.id ORDER BY rf.created_at DESC LIMIT 1),'') FROM settlement_tax_invoice ti JOIN settlement_invoice_request ir ON ir.id=ti.invoice_request_id AND ir.tenant_id=ti.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id WHERE ti.tenant_id=? ORDER BY ti.issue_date DESC,ti.created_at DESC,ti.id DESC LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, requestID, code, number, contract, kind, amount, issueDate, status, channel, redFlushStatus string
		var buyer []byte
		if err := rows.Scan(&id, &requestID, &code, &number, &contract, &buyer, &kind, &amount, &issueDate, &status, &channel, &redFlushStatus); err != nil {
			respondError(w, err)
			return
		}
		buyerName, buyerTaxNo := buyerSummary(buyer)
		if !taxNumberVisible {
			buyerTaxNo = ""
		}
		items = append(items, map[string]any{"id": id, "invoice_request_id": requestID, "invoice_code": code, "invoice_no": number, "contract_no": contract, "buyer_name": buyerName, "buyer_tax_no_masked": maskTaxNo(buyerTaxNo), "invoice_type": kind, "amount_incl_tax": amount, "issue_date": issueDate, "status": status, "issued_by_channel": channel, "red_flush_status": redFlushStatus})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

type taxInvoiceDetailLine struct {
	LineNo                int
	ItemName              string
	TaxClassificationCode string
	AmountExclTax         string
	TaxRate               string
	TaxAmount             string
	AmountInclTax         string
}

// taxInvoiceDetailLines 读取一个已存储明细集合。调用方负责
// 选择表：已开税票明细与申请明细属于不同
// 事实，不能相互替代。
func (a *API) taxInvoiceDetailLines(ctx context.Context, query, tenant, sourceID string) ([]taxInvoiceDetailLine, error) {
	rows, err := a.service.DB.QueryContext(ctx, query, tenant, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := []taxInvoiceDetailLine{}
	for rows.Next() {
		var line taxInvoiceDetailLine
		if err := rows.Scan(&line.LineNo, &line.ItemName, &line.TaxClassificationCode, &line.AmountExclTax, &line.TaxRate, &line.TaxAmount, &line.AmountInclTax); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

func invoiceDetailLinePayloads(lines []taxInvoiceDetailLine) []map[string]any {
	items := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		items = append(items, map[string]any{
			"line_no": line.LineNo, "item_name": line.ItemName,
			"tax_classification_code": line.TaxClassificationCode,
			"amount_excl_tax":         line.AmountExclTax, "tax_rate_display": taxRateDisplay(line.TaxRate, 1),
			"tax_amount": line.TaxAmount, "amount_incl_tax": line.AmountInclTax,
		})
	}
	return items
}

func invoiceDecimal(value string) (*big.Rat, bool) {
	number, ok := new(big.Rat).SetString(strings.TrimSpace(value))
	return number, ok
}

// verifyTaxInvoiceDetailLines 将展示的已开明细与发票头
// 关联校验。红票头合计为负，因此每条已开明细
// 必须带相同符号（零税率税额分量可保持零）；
// 明细合计还必须与票头合计精确相等。
func verifyTaxInvoiceDetailLines(headerExcl, headerTax, headerIncl string, lines []taxInvoiceDetailLine) error {
	if len(lines) == 0 {
		return errors.New("tax invoice has no issued items")
	}
	headerInclAmount, ok := invoiceDecimal(headerIncl)
	if !ok {
		return errors.New("invalid invoice header amount")
	}
	headerSign := headerInclAmount.Sign()
	totalExcl, totalTax, totalIncl := new(big.Rat), new(big.Rat), new(big.Rat)
	for index, line := range lines {
		excl, okExcl := invoiceDecimal(line.AmountExclTax)
		tax, okTax := invoiceDecimal(line.TaxAmount)
		incl, okIncl := invoiceDecimal(line.AmountInclTax)
		if !okExcl || !okTax || !okIncl {
			return fmt.Errorf("line %d has an invalid amount", index+1)
		}
		if headerSign < 0 && (excl.Sign() > 0 || tax.Sign() > 0 || incl.Sign() > 0) {
			return fmt.Errorf("line %d has a positive amount under a negative invoice header", index+1)
		}
		if headerSign > 0 && (excl.Sign() < 0 || tax.Sign() < 0 || incl.Sign() < 0) {
			return fmt.Errorf("line %d has a negative amount under a positive invoice header", index+1)
		}
		totalExcl.Add(totalExcl, excl)
		totalTax.Add(totalTax, tax)
		totalIncl.Add(totalIncl, incl)
	}
	wantExcl, okExcl := invoiceDecimal(headerExcl)
	wantTax, okTax := invoiceDecimal(headerTax)
	if !okExcl || !okTax {
		return errors.New("invalid invoice header amount")
	}
	if totalExcl.Cmp(wantExcl) != 0 || totalTax.Cmp(wantTax) != 0 || totalIncl.Cmp(headerInclAmount) != 0 {
		return errors.New("issued item totals differ from the invoice header")
	}
	return nil
}

// taxInvoiceDetail 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) taxInvoiceDetail(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.read")
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/tax-invoices/")
	if id == "" || strings.Contains(id, "/") {
		fail(w, http.StatusNotFound, "TAX_INVOICE_NOT_FOUND", "发票不存在")
		return
	}
	if !a.recordAllowed(w, r, p, "settlement.invoice.read", "invoice", id) {
		return
	}
	taxNumberVisible := true
	if p.CatalogVersion != "" {
		var policyErr error
		taxNumberVisible, policyErr = a.service.TaxNumberVisible(r.Context(), p.TenantID)
		if policyErr != nil {
			respondError(w, policyErr)
			return
		}
	}
	var invoiceID, requestID, requestNo, code, number, contract, kind, excl, tax, incl, issueDate, status, channel string
	var buyer, seller []byte
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT ti.id,ti.invoice_request_id,ir.request_no,ti.invoice_code,ti.invoice_no,cs.source_contract_no,ti.buyer_profile_snapshot,ti.seller_profile_snapshot,ti.invoice_type,ti.amount_excl_tax,ti.tax_amount,ti.amount_incl_tax,DATE_FORMAT(ti.issue_date,'%Y-%m-%d'),ti.status,ti.issued_by_channel FROM settlement_tax_invoice ti JOIN settlement_invoice_request ir ON ir.id=ti.invoice_request_id AND ir.tenant_id=ti.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id WHERE ti.id=? AND ti.tenant_id=?`, id, p.TenantID).Scan(&invoiceID, &requestID, &requestNo, &code, &number, &contract, &buyer, &seller, &kind, &excl, &tax, &incl, &issueDate, &status, &channel)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "TAX_INVOICE_NOT_FOUND", "发票不存在")
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	buyerName, buyerTaxNo := buyerSummary(buyer)
	if !taxNumberVisible {
		buyerTaxNo = ""
	}
	sellerName, _ := buyerSummary(seller)
	issuedLines, err := a.taxInvoiceDetailLines(r.Context(), `SELECT line_no,item_name,tax_classification_code,amount_excl_tax,tax_rate,tax_amount,amount_incl_tax FROM settlement_tax_invoice_item WHERE tenant_id=? AND tax_invoice_id=? ORDER BY line_no`, p.TenantID, invoiceID)
	if err != nil {
		respondError(w, err)
		return
	}
	requestLines, err := a.taxInvoiceDetailLines(r.Context(), `SELECT line_no,item_name,tax_classification_code,amount_excl_tax,tax_rate,tax_amount,amount_incl_tax FROM settlement_invoice_request_item WHERE tenant_id=? AND invoice_request_id=? ORDER BY line_no`, p.TenantID, requestID)
	if err != nil {
		respondError(w, err)
		return
	}
	items := issuedLines
	itemsSource := "TAX_INVOICE"
	if len(issuedLines) == 0 {
		// 旧版人工发票没有存储税票明细。对这些记录保留
		// 原蓝票展示行为，但负数红票的已开符号具有业务意义，
		// 绝不能使用此回退。
		headerInclAmount, ok := invoiceDecimal(incl)
		if !ok || headerInclAmount.Sign() < 0 {
			fail(w, http.StatusConflict, "SETTLEMENT_TAX_INVOICE_ITEM_MISMATCH", "红票缺少票面明细，无法与票头核对")
			return
		}
		items = requestLines
		itemsSource = "REQUEST"
	} else if err := verifyTaxInvoiceDetailLines(excl, tax, incl, issuedLines); err != nil {
		fail(w, http.StatusConflict, "SETTLEMENT_TAX_INVOICE_ITEM_MISMATCH", "发票票面明细与票头金额或符号不一致，暂不展示")
		return
	}
	documentRows, err := a.service.DB.QueryContext(r.Context(), `SELECT id,document_type,original_name,media_type,archived_at FROM settlement_invoice_document WHERE tenant_id=? AND tax_invoice_id=? ORDER BY archived_at DESC`, p.TenantID, invoiceID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer documentRows.Close()
	documents := []map[string]any{}
	for documentRows.Next() {
		var documentID, documentType, originalName, mediaType string
		var archivedAt any
		if err := documentRows.Scan(&documentID, &documentType, &originalName, &mediaType, &archivedAt); err != nil {
			respondError(w, err)
			return
		}
		documents = append(documents, map[string]any{"id": documentID, "document_type": documentType, "original_name": originalName, "media_type": mediaType, "archived_at": archivedAt})
	}
	if err := documentRows.Err(); err != nil {
		respondError(w, err)
		return
	}
	var redFlushID, redFlushStatus, redFlushReason, redFlushRequestedBy, redFlushReviewReason string
	var redFlushVersion int
	redErr := a.service.DB.QueryRowContext(r.Context(), `SELECT id,status,reason_detail,requested_by,review_reason,version FROM settlement_invoice_red_flush_request WHERE tenant_id=? AND original_invoice_id=? ORDER BY created_at DESC LIMIT 1`, p.TenantID, invoiceID).Scan(&redFlushID, &redFlushStatus, &redFlushReason, &redFlushRequestedBy, &redFlushReviewReason, &redFlushVersion)
	if redErr != nil && !errors.Is(redErr, sql.ErrNoRows) {
		respondError(w, redErr)
		return
	}
	var redFlush any
	if redErr == nil {
		redFlush = map[string]any{"id": redFlushID, "status": redFlushStatus, "reason_detail": redFlushReason, "review_reason": redFlushReviewReason, "version": redFlushVersion, "can_review": redFlushStatus == "SUBMITTED" && redFlushRequestedBy != p.UserID && p.Allows("settlement.invoice.approve")}
	}
	write(w, http.StatusOK, map[string]any{"id": invoiceID, "invoice_request_id": requestID, "request_no": requestNo, "invoice_code": code, "invoice_no": number, "contract_no": contract, "buyer_name": buyerName, "buyer_tax_no_masked": maskTaxNo(buyerTaxNo), "seller_name": sellerName, "invoice_type": kind, "amount_excl_tax": excl, "tax_amount": tax, "amount_incl_tax": incl, "issue_date": issueDate, "status": status, "issued_by_channel": channel, "document_count": len(documents), "documents": documents, "red_flush_request": redFlush, "items": invoiceDetailLinePayloads(items), "items_source": itemsSource, "request_items": invoiceDetailLinePayloads(requestLines)})
}

func (a *API) uploadInvoiceDocument(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.issue")
	if !ok {
		return
	}
	if a.files == nil || a.cfg.FileGatewayApplicationID == "" {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_FILE_GATEWAY_UNAVAILABLE", "电子发票文件服务尚未配置")
		return
	}
	invoiceID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/tax-invoices/"), "/documents")
	if invoiceID == "" || strings.Contains(invoiceID, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "发票标识不合法")
		return
	}
	var invoiceExists int
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM settlement_tax_invoice WHERE id=? AND tenant_id=?`, invoiceID, p.TenantID).Scan(&invoiceExists); err != nil {
		respondError(w, err)
		return
	}
	if invoiceExists == 0 {
		fail(w, http.StatusNotFound, "SETTLEMENT_TAX_INVOICE_NOT_FOUND", "发票不存在")
		return
	}
	const maxInvoiceDocumentBytes = 10 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxInvoiceDocumentBytes+(1<<20))
	if err := r.ParseMultipartForm(maxInvoiceDocumentBytes); err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "电子发票文件不得超过 10 MiB")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "请选择电子发票文件")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxInvoiceDocumentBytes+1))
	if err != nil || len(content) == 0 || len(content) > maxInvoiceDocumentBytes {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "电子发票文件为空或超过 10 MiB")
		return
	}
	originalName := filepath.Base(strings.TrimSpace(header.Filename))
	extension := strings.ToLower(filepath.Ext(originalName))
	allowedExtensions := map[string]bool{".pdf": true, ".ofd": true, ".xml": true, ".png": true, ".jpg": true, ".jpeg": true}
	if originalName == "." || originalName == "" || len(originalName) > 255 || !allowedExtensions[extension] {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "仅支持 PDF、OFD、XML、PNG 或 JPG 电子发票文件")
		return
	}
	detectedType, _, _ := mime.ParseMediaType(http.DetectContentType(content))
	mediaTypeByExtension := map[string]string{".pdf": "application/pdf", ".ofd": "application/ofd", ".xml": "application/xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg"}
	validContent := map[string]map[string]bool{
		".pdf":  {"application/pdf": true},
		".ofd":  {"application/zip": true},
		".xml":  {"application/xml": true, "text/xml": true, "text/plain": true},
		".png":  {"image/png": true},
		".jpg":  {"image/jpeg": true},
		".jpeg": {"image/jpeg": true},
	}
	if !validContent[extension][detectedType] {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "电子发票扩展名与文件内容不一致")
		return
	}
	mediaType := mediaTypeByExtension[extension]
	documentType := strings.TrimSpace(r.FormValue("document_type"))
	if documentType == "" {
		documentType = "ELECTRONIC_INVOICE"
	}
	if documentType != "ELECTRONIC_INVOICE" && documentType != "INVOICE_IMAGE" {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "电子发票文件类型不合法")
		return
	}
	checksum := fmt.Sprintf("%x", sha256.Sum256(content))
	requestID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if requestID == "" {
		fail(w, http.StatusBadRequest, "SETTLEMENT_IDEMPOTENCY_KEY_REQUIRED", "上传电子发票需要幂等键")
		return
	}
	documentID, created, err := a.service.UploadInvoiceDocument(r.Context(), p,
		service.InvoiceArchiveIdentity{TenantID: a.cfg.OIDCTenantID, ApplicationID: a.cfg.FileGatewayApplicationID, GatewayURL: a.cfg.FileGatewayURL}, a.files,
		service.InvoiceArchiveInput{Key: requestID, InvoiceID: invoiceID, DocumentType: documentType, OriginalName: originalName, MediaType: mediaType, Checksum: checksum, Content: content})
	if err != nil {
		if errors.Is(err, service.ErrInvoiceArchiveLicense) {
			fail(w, http.StatusForbidden, "COMMERCIAL_LICENSE_DENIED", "商业授权不可用或已到期；历史查询与导出仍受原权限控制")
			return
		}
		if errors.Is(err, service.ErrInvoiceArchiveProvenance) {
			fail(w, http.StatusConflict, "SETTLEMENT_INVOICE_ARCHIVE_IDENTITY_UNVERIFIED", "已存在归档文件，但无法确认其原文件服务归属或当前配置不匹配；请管理员受控核对归档来源，勿反复重试")
			return
		}
		if errors.Is(err, service.ErrInvoiceArchiveGateway) {
			fail(w, http.StatusBadGateway, "SETTLEMENT_FILE_ARCHIVE_FAILED", "电子发票归档失败，请稍后重试")
			return
		}
		respondError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	write(w, status, map[string]string{"id": documentID, "status": "ARCHIVED"})
}

// downloadInvoiceDocument 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) downloadInvoiceDocument(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.read")
	if !ok {
		return
	}
	if a.files == nil {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_FILE_GATEWAY_UNAVAILABLE", "电子发票文件服务尚未配置")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/tax-invoices/")
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[1] != "documents" || parts[3] != "download" {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "电子发票文件标识不合法")
		return
	}
	if !a.recordAllowed(w, r, p, "settlement.invoice.read", "invoice", parts[0]) {
		return
	}
	var objectID, originalName, mediaType string
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT storage_object_id,original_name,media_type FROM settlement_invoice_document WHERE tenant_id=? AND tax_invoice_id=? AND id=?`, p.TenantID, parts[0], parts[2]).Scan(&objectID, &originalName, &mediaType)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "SETTLEMENT_INVOICE_DOCUMENT_NOT_FOUND", "电子发票文件不存在")
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	content, err := a.files.Download(r.Context(), objectID)
	if err != nil {
		fail(w, http.StatusBadGateway, "SETTLEMENT_FILE_DOWNLOAD_FAILED", "电子发票下载失败，请稍后重试")
		return
	}
	if err = a.service.AuditAction(r.Context(), p, "SETTLEMENT_INVOICE_DOCUMENT_DOWNLOADED", "invoice_document", parts[2], map[string]any{"invoice_id": parts[0], "risk_level": "HIGH"}); err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": originalName}))
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (a *API) requestInvoiceRedFlush(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.issue")
	if !ok {
		return
	}
	invoiceID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/tax-invoices/"), "/red-flush")
	if invoiceID == "" || strings.Contains(invoiceID, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "发票标识不合法")
		return
	}
	var in service.InvoiceRedFlushInput
	if !decode(w, r, &in) {
		return
	}
	id, err := a.service.RequestInvoiceRedFlush(r.Context(), p, r.Header.Get("Idempotency-Key"), invoiceID, in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusAccepted, map[string]string{"id": id, "status": "SUBMITTED"})
}

func (a *API) reviewInvoiceRedFlush(w http.ResponseWriter, r *http.Request, approve bool) {
	p, ok := a.user(w, r, "settlement.invoice.approve")
	if !ok {
		return
	}
	suffix := "/reject"
	if approve {
		suffix = "/approve"
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-red-flush-requests/"), suffix)
	if id == "" || strings.Contains(id, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "红冲申请标识不合法")
		return
	}
	var in service.InvoiceRedFlushReviewInput
	if !decode(w, r, &in) {
		return
	}
	if err := a.service.ReviewInvoiceRedFlush(r.Context(), p, id, approve, in); err != nil {
		respondError(w, err)
		return
	}
	status := "REJECTED"
	if approve {
		status = "ISSUE_PENDING"
	}
	write(w, http.StatusOK, map[string]string{"status": status})
}

func buyerSummary(raw []byte) (string, string) {
	var buyer struct {
		Name  string `json:"name"`
		TaxNo string `json:"tax_no"`
	}
	_ = json.Unmarshal(raw, &buyer)
	return buyer.Name, buyer.TaxNo
}

func maskTaxNo(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 4 {
		return value
	}
	if len(value) <= 8 {
		return value[:2] + "****" + value[len(value)-2:]
	}
	return value[:6] + "****" + value[len(value)-2:]
}

func taxRateDisplay(value string, count int) string {
	if count == 0 || strings.TrimSpace(value) == "" {
		return "—"
	}
	if count > 1 {
		return "多税率"
	}
	rate, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return "—"
	}
	return fmt.Sprintf("%g%%", rate*100)
}
func (a *API) createDunningPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.manage")
	if !ok {
		return
	}
	var in service.DunningPolicyInput
	if !decode(w, r, &in) {
		return
	}
	recipient := strings.TrimSpace(strings.TrimPrefix(in.RecipientRule, "USER:"))
	if recipient == "" || recipient == in.RecipientRule {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "催收提醒人员无效")
		return
	}
	if a.directory == nil && !a.cfg.DevelopmentAuth {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_PERSONNEL_DIRECTORY_UNAVAILABLE", "催收人员目录尚未配置")
		return
	}
	if a.directory != nil {
		eligible, err := a.directory.ContainsCollector(r.Context(), recipient)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_PERSONNEL_DIRECTORY_UNAVAILABLE", "催收人员目录暂不可用")
			return
		}
		if !eligible {
			fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_RECIPIENT", "提醒人员必须是当前系统中有效的催收人员")
			return
		}
	}
	id, err := a.service.CreateDunningPolicy(r.Context(), p, in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": id})
}

func (a *API) listDunningRecipients(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.user(w, r, "settlement.dunning.manage"); !ok {
		return
	}
	if a.directory == nil {
		if a.cfg.DevelopmentAuth {
			write(w, http.StatusOK, map[string]any{"items": []map[string]any{{"user_id": "dev-finance", "display_name": "本地结算管理员", "roles": []string{"settlement_collector"}}}, "page": 1, "page_size": 50, "total": 1})
			return
		}
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_PERSONNEL_DIRECTORY_UNAVAILABLE", "催收人员目录尚未配置")
		return
	}
	page, pageSize := 1, 50
	if value := strings.TrimSpace(r.URL.Query().Get("page")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100000 {
			fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "页码无效")
			return
		}
		page = parsed
	}
	if value := strings.TrimSpace(r.URL.Query().Get("page_size")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "分页大小无效")
			return
		}
		pageSize = parsed
	}
	items, total, err := a.directory.ListCollectors(r.Context(), r.URL.Query().Get("keyword"), page, pageSize)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_PERSONNEL_DIRECTORY_UNAVAILABLE", "催收人员目录暂不可用")
		return
	}
	write(w, http.StatusOK, map[string]any{"items": items, "page": page, "page_size": pageSize, "total": total})
}
func (a *API) listDunningPolicies(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.manage")
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled FROM settlement_dunning_policy WHERE tenant_id=? ORDER BY aging_from_days,id LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, action, recipient, channel, priority string
		var version, from, to, repeat int
		var enabled bool
		if err := rows.Scan(&id, &name, &version, &from, &to, &action, &recipient, &channel, &repeat, &priority, &enabled); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "version": version, "aging_from_days": from, "aging_to_days": to, "action_type": action, "recipient_rule": recipient, "channel": channel, "repeat_interval_days": repeat, "priority": priority, "enabled": enabled})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) dunningOverview(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.manage")
	if !ok {
		return
	}
	overview, err := a.service.DunningOverview(r.Context(), p)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, overview)
}

func (a *API) dunningCoverage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.manage")
	if !ok {
		return
	}
	coverage, err := a.service.DunningCoverage(r.Context(), p)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, coverage)
}

// completeDunningAction 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) completeDunningAction(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.feedback")
	if !ok {
		return
	}
	actionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/dunning/actions/"), "/complete")
	if actionID == "" || strings.Contains(actionID, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "催收动作标识不合法")
		return
	}
	var in struct {
		Version uint64 `json:"version"`
		Note    string `json:"note"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := a.service.CompleteDunningAction(r.Context(), p, actionID, in.Version, in.Note); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]string{"id": actionID, "status": "COMPLETED"})
}

// listDunningCases 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) listDunningCases(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.read")
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	rows, err := a.queryScoped(r.Context(), p, "settlement.dunning.read", `SELECT dc.id,COALESCE(r.receivable_no,CONCAT('PLAN-',rp.installment_no)),cs.customer_name_snapshot,DATEDIFF(CURDATE(),COALESCE(r.due_date,rp.due_date)),COALESCE(r.open_amount,rp.planned_amount),dc.current_escalation_level,dc.status,dc.next_action_at,dc.last_action_at,COALESCE((SELECT da.recipient_user_id FROM settlement_dunning_action da WHERE da.tenant_id=dc.tenant_id AND da.dunning_case_id=dc.id ORDER BY da.created_at DESC,da.id DESC LIMIT 1),'') FROM settlement_dunning_case dc LEFT JOIN settlement_receivable r ON r.id=dc.receivable_id AND r.tenant_id=dc.tenant_id LEFT JOIN settlement_receivable_plan rp ON rp.id=dc.plan_id AND rp.tenant_id=dc.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=COALESCE(r.contract_snapshot_id,rp.contract_snapshot_id) AND cs.tenant_id=dc.tenant_id WHERE dc.tenant_id=? ORDER BY dc.next_action_at,dc.id LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, customer, open, status, recipient string
		var days, level int
		var next, last any
		if err := rows.Scan(&id, &no, &customer, &days, &open, &level, &status, &next, &last, &recipient); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "receivable_no": no, "customer_name": customer, "aging_days": days, "open_amount": open, "level": level, "status": status, "next_action_at": next, "last_action_at": last, "recipient_user_id": recipient})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

// listDunningActions 用途：按可信权限及责任范围处理结算请求。
// 参数：w 为HTTP响应，r 为携带当前会话的请求。
// 返回值：无，结果通过HTTP响应返回。
func (a *API) listDunningActions(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.read")
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	rows, err := a.queryScoped(r.Context(), p, "settlement.dunning.read", `SELECT da.id,COALESCE(r.receivable_no,CONCAT('PLAN-',rp.installment_no)),da.channel,da.sent_at,da.created_at,da.recipient_user_id,da.priority,da.status,dc.current_escalation_level,da.version,da.completed_at,da.completed_by,da.result_detail FROM settlement_dunning_action da JOIN settlement_dunning_case dc ON dc.id=da.dunning_case_id AND dc.tenant_id=da.tenant_id LEFT JOIN settlement_receivable r ON r.id=dc.receivable_id AND r.tenant_id=dc.tenant_id LEFT JOIN settlement_receivable_plan rp ON rp.id=dc.plan_id AND rp.tenant_id=dc.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=COALESCE(r.contract_snapshot_id,rp.contract_snapshot_id) AND cs.tenant_id=dc.tenant_id WHERE da.tenant_id=? ORDER BY COALESCE(da.sent_at,da.created_at) DESC,da.id DESC LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, receivableNo, channel, recipient, priority, status, completedBy, resultDetail string
		var sentAt, createdAt, completedAt any
		var level int
		var version uint64
		if err := rows.Scan(&id, &receivableNo, &channel, &sentAt, &createdAt, &recipient, &priority, &status, &level, &version, &completedAt, &completedBy, &resultDetail); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "receivable_no": receivableNo, "channel": channel, "sent_at": sentAt, "created_at": createdAt, "recipient_user_id": recipient, "priority": priority, "status": status, "escalation_level": level, "version": version, "completed_at": completedAt, "completed_by": completedBy, "result_detail": resultDetail})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) listNotifications(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.notification.read")
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	if r.URL.Query().Get("limit") == "" {
		limit = 50
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT id,notification_scope,priority,title,content,target_url,reference_type,reference_id,created_at,read_at FROM settlement_local_notification WHERE tenant_id=? AND recipient_user_id=? ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, p.TenantID, p.UserID, limit, offset)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, scope, priority, title, content, target, refType, refID string
		var created any
		var read sql.NullTime
		if err := rows.Scan(&id, &scope, &priority, &title, &content, &target, &refType, &refID, &created, &read); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "scope": scope, "priority": priority, "title": title, "content": content, "target_url": target, "reference_type": refType, "reference_id": refID, "created_at": created, "read": read.Valid})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) unreadCount(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.notification.read")
	if !ok {
		return
	}
	var count int
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM settlement_local_notification WHERE tenant_id=? AND recipient_user_id=? AND read_at IS NULL`, p.TenantID, p.UserID).Scan(&count); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]int{"unread_count": count})
}
func (a *API) readNotification(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.notification.read")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/notifications/"), "/read")
	result, err := a.service.DB.ExecContext(r.Context(), `UPDATE settlement_local_notification SET read_at=COALESCE(read_at,UTC_TIMESTAMP(3)) WHERE id=? AND tenant_id=? AND recipient_user_id=?`, id, p.TenantID, p.UserID)
	if err != nil {
		respondError(w, err)
		return
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到通知")
		return
	}
	write(w, http.StatusOK, map[string]string{"status": "read"})
}

// user 用途：按结算权限、责任范围和错误契约处理请求。
// 参数：包含当前请求、可信主体或显式服务依赖；不接受客户端声明扩大权限。
// 返回值：返回处理器、主体或HTTP结果，错误保持失败关闭。
func (a *API) user(w http.ResponseWriter, r *http.Request, permission string) (service.Principal, bool) {
	p, err := a.principal(r)
	if err != nil {
		if errors.Is(err, platform.ErrUnauthenticated) {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_UNAUTHENTICATED", "登录已失效，请重新登录")
		} else {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_IDENTITY_UNAVAILABLE", "身份服务暂不可用")
		}
		return p, false
	}
	if !p.Allows(permission) {
		fail(w, http.StatusForbidden, "SETTLEMENT_FORBIDDEN", "当前用户没有执行此操作的权限")
		return p, false
	}
	switch permission {
	case "settlement.receipt.record", "settlement.receipt.read", "settlement.allocation.read", "settlement.allocation.confirm", "settlement.allocation.reverse", "settlement.invoice.approve", "settlement.invoice.issue", "settlement.invoice.special_approve", "settlement.settings.manage", "settlement.collaboration.manage":
		if !p.Unrestricted(permission) {
			fail(w, http.StatusForbidden, "SETTLEMENT_FORBIDDEN", "当前角色的数据范围不允许执行此财务操作")
			return p, false
		}
	}
	return p, true
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "请求内容不合法")
		return false
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

// respondError 用途：按结算权限、责任范围和错误契约处理请求。
// 参数：包含当前请求、可信主体或显式服务依赖；不接受客户端声明扩大权限。
// 返回值：返回处理器、主体或HTTP结果，错误保持失败关闭。
func respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrDependency):
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_DEPENDENCY_UNAVAILABLE", "人员目录或项目服务暂不可用，请稍后重试")
	case errors.Is(err, service.ErrInvalid):
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
	case errors.Is(err, service.ErrNotFound):
		fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到记录")
	case errors.Is(err, service.ErrConflict):
		fail(w, http.StatusConflict, "SETTLEMENT_CONFLICT", "记录已变化、重复或余额不足")
	case errors.Is(err, service.ErrForbidden):
		fail(w, http.StatusForbidden, "SETTLEMENT_FORBIDDEN", "当前用户无权执行此操作")
	default:
		fail(w, http.StatusInternalServerError, "SETTLEMENT_INTERNAL_ERROR", "服务处理失败")
	}
}
func write(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}
