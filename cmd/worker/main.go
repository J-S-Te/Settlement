package main

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/J-S-Te/license-core/consumer"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/creditclient"
	"github.com/j-s-te/settlement/internal/dunning"
	"github.com/j-s-te/settlement/internal/filegatewayclient"
	"github.com/j-s-te/settlement/internal/outbox"
	platformdir "github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/projectreference"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/internal/taxreconcile"
	"golang.org/x/oauth2/clientcredentials"
)

// main 用途：装配结算投递、导出、回单恢复和负责人提醒消费者。
// 参数：无，读取专属租户及机器身份配置。
// 返回值：无，失败记录日志并保留持久任务供重试。
func main() {
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_MYSQL_DSN"))
	platform := strings.TrimRight(strings.TrimSpace(os.Getenv("PLATFORM_BASE_URL")), "/")
	environment := strings.TrimSpace(os.Getenv("PLATFORM_ENVIRONMENT_CODE"))
	if environment == "" {
		environment = "dev"
	}
	if dsn == "" || platform == "" {
		slog.Error("SETTLEMENT_MYSQL_DSN and PLATFORM_BASE_URL are required")
		os.Exit(1)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	host, _ := os.Hostname()
	workerID := host + "-" + time.Now().UTC().Format("20060102150405.000000000")
	destinations := []outbox.Destination{}
	var degradedDestinations []outbox.DegradedDestination
	var taxReconciler *taxreconcile.Worker
	issuanceMode := strings.ToLower(strings.TrimSpace(os.Getenv("SETTLEMENT_INVOICE_ISSUANCE_MODE")))
	if issuanceMode == "" {
		issuanceMode = "manual"
	}
	if issuanceMode != "manual" && issuanceMode != "tax_adapter" {
		slog.Error("SETTLEMENT_INVOICE_ISSUANCE_MODE must be manual or tax_adapter")
		os.Exit(1)
	}
	// BIZ-XAPI-10：审计目的地按“显式开关 > 生产环境”判定，生产缺失即失败关闭；
	// 显式开关由 config.AuditRequiredSwitch 读取（SETTLEMENT_AUDIT_REQUIRED 优先，
	// 缺失时兼容平台统一键 PLATFORM_AUDIT_REQUIRED）。
	// 通知目的地缺失只登记 degraded，不阻断 worker 启动，积压由 /readyz、/metrics 与扫描告警暴露。
	auditRequired, auditRequiredErr := config.AuditDestinationRequired(
		config.AuditRequiredSwitch(),
		os.Getenv("APP_ENV"),
		environment,
	)
	if auditRequiredErr != nil {
		slog.Error("invalid settlement audit destination switch", "error", auditRequiredErr)
		os.Exit(1)
	}
	destinationPlan, destinationErr := outbox.PlanPlatformDestinations(outbox.PlatformDestinationInput{
		PlatformBaseURL:          platform,
		EnvironmentCode:          environment,
		ApplicationCode:          "settlement",
		NotificationClientID:     os.Getenv("SETTLEMENT_NOTIFICATION_CLIENT_ID"),
		NotificationClientSecret: os.Getenv("SETTLEMENT_NOTIFICATION_CLIENT_SECRET"),
		AuditClientID:            os.Getenv("SETTLEMENT_AUDIT_CLIENT_ID"),
		AuditClientSecret:        os.Getenv("SETTLEMENT_AUDIT_CLIENT_SECRET"),
		AuditRequired:            auditRequired,
	})
	if destinationErr != nil {
		slog.Error("settlement integration destination configuration failed", "error", destinationErr)
		os.Exit(1)
	}
	for _, warning := range destinationPlan.Warnings {
		slog.Warn(warning)
	}
	destinations = append(destinations, destinationPlan.Destinations...)
	degradedDestinations = append(degradedDestinations, destinationPlan.Degraded...)
	if issuanceMode == "tax_adapter" {
		endpoint, statusURL, id, secret := strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_ADAPTER_URL")), strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_STATUS_URL")), strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_CLIENT_ID")), os.Getenv("SETTLEMENT_TAX_CLIENT_SECRET")
		if endpoint == "" || statusURL == "" || id == "" || secret == "" {
			slog.Error("tax adapter command/status URLs and credentials are required in tax_adapter issuance mode")
			os.Exit(1)
		}
		tokenURL := strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_TOKEN_URL"))
		if tokenURL == "" {
			tokenURL = platform + "/oauth2/token"
		}
		destinations = append(destinations, outbox.Destination{Name: "TAX_INVOICE_COMMAND", Endpoint: endpoint, TokenEndpoint: tokenURL, Scope: "invoice.issue", ClientID: id, ClientSecret: secret, ApplicationCode: "settlement", EnvironmentCode: environment})
		tokenConfig := &clientcredentials.Config{ClientID: id, ClientSecret: secret, TokenURL: tokenURL, Scopes: []string{"invoice.status.read"}}
		taxReconciler = &taxreconcile.Worker{DB: db, Service: &service.Service{DB: db, InvoiceIssuanceMode: issuanceMode, TaxProviderCode: strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_PROVIDER_CODE"))}, HTTP: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, StatusURL: statusURL, AccessToken: func(ctx context.Context) (string, error) {
			token, tokenErr := tokenConfig.Token(ctx)
			if tokenErr != nil {
				return "", tokenErr
			}
			return token.AccessToken, nil
		}, BatchSize: 50}
	}
	var creditWorker *service.CreditSyncWorker
	var creditPublisher *creditclient.Client
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SETTLEMENT_CRM_CREDIT_SYNC_ENABLED")), "true") {
		endpoint := strings.TrimSpace(os.Getenv("CRM_CREDIT_SYNC_ENDPOINT"))
		tokenURL := strings.TrimSpace(os.Getenv("CRM_CREDIT_TOKEN_URL"))
		if tokenURL == "" {
			tokenURL = platform + "/oauth2/token"
		}
		clientID := strings.TrimSpace(os.Getenv("CRM_CREDIT_CLIENT_ID"))
		clientSecret := os.Getenv("CRM_CREDIT_CLIENT_SECRET")
		// scope 与平台受控用途 crm_credit_payment_ingest 及 CRM 机器权限白名单
		// 必须完全一致；这里拒绝环境文件里的历史名字，避免 worker 继续 401。
		scope := strings.TrimSpace(os.Getenv("CRM_CREDIT_SCOPE"))
		if scope == "" {
			scope = config.CRMCreditPaymentIngestScope
		}
		if scope != config.CRMCreditPaymentIngestScope {
			slog.Error("CRM_CREDIT_SCOPE does not match the CRM machine permission and platform purpose binding",
				"scope", scope, "required_scope", config.CRMCreditPaymentIngestScope)
			os.Exit(1)
		}
		var creditErr error
		creditPublisher, creditErr = creditclient.New(endpoint, tokenURL, clientID, clientSecret, scope, &http.Client{Timeout: 8 * time.Second})
		if creditErr != nil {
			slog.Error("initialize CRM credit retry publisher", "error", creditErr)
			os.Exit(1)
		}
		creditWorker = &service.CreditSyncWorker{DB: db, Publisher: creditPublisher, BatchSize: 50, Lease: 2 * time.Minute}
	}
	httpClient := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	worker := &outbox.Worker{Store: &outbox.Store{DB: db, WorkerID: workerID, Lease: 30 * time.Second}, HTTP: httpClient, TokenEndpoint: platform + "/oauth2/token", BatchSize: 50, Destinations: destinations, DegradedDestinations: degradedDestinations}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	commercialGate, licenseErr := consumer.FromEnvironment("settlement")
	if licenseErr != nil {
		slog.Error("commercial license configuration failed")
		os.Exit(1)
	}
	go func() {
		_ = commercialGate.Run(ctx, func(error) {
			slog.Warn("commercial license synchronization unavailable; local expiry remains authoritative")
		})
	}()
	checkBusiness := func(ctx context.Context) error { return commercialGate.Check(ctx, "MUTATE_BUSINESS") }
	worker.CheckBusinessLicense = checkBusiness
	if creditWorker != nil {
		creditWorker.CheckBusinessLicense = checkBusiness
		creditPublisher.SetBusinessLicenseCheck(checkBusiness)
	}
	if taxReconciler != nil {
		taxReconciler.CheckBusinessLicense = checkBusiness
	}
	scanner := &dunning.Scanner{DB: db, BatchSize: 50}
	scanner.CheckBusinessLicense = checkBusiness
	scanner.Directory = platformdir.NewPersonnelDirectory(
		os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_URL"),
		os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_CLIENT_ID"),
		os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_CLIENT_SECRET"),
		5*time.Second,
	)
	roleReminder := &dunning.RoleReminderWorker{DB: db, TenantID: strings.TrimSpace(os.Getenv("OIDC_TENANT_ID")), Service: &service.Service{DB: db}, CheckBusinessLicense: checkBusiness, BatchSize: 50}
	if directory, ok := scanner.Directory.(dunning.RoleReminderDirectory); ok {
		roleReminder.Directory = directory
	}
	// BIZ-XAPI-05b：项目引用快照周期刷新。项目换合同/失效后由本刷新器按权威
	// 项目服务收口 settlement_project_reference.active，避免助理继续读写旧合同范围。
	// 机器身份与 API 共用 SETTLEMENT_PROJECT_REFERENCE_*；未配置时只告警并关闭
	// 刷新器，不阻断投递、导出与对账等主流程启动。
	projectReferenceSyncInterval, projectReferenceSyncBatch, syncConfigErr := config.ProjectReferenceSyncSettings()
	if syncConfigErr != nil {
		slog.Error("project reference sync configuration failed", "error", syncConfigErr)
		os.Exit(1)
	}
	var projectReferenceSync *service.ProjectReferenceSyncWorker
	if projectReferenceSyncInterval > 0 {
		referenceEndpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("SETTLEMENT_PROJECT_REFERENCE_URL")), "/")
		referenceTokenURL := strings.TrimSpace(os.Getenv("SETTLEMENT_PROJECT_REFERENCE_TOKEN_URL"))
		referenceClientID := strings.TrimSpace(os.Getenv("SETTLEMENT_PROJECT_REFERENCE_CLIENT_ID"))
		referenceSecret := os.Getenv("SETTLEMENT_PROJECT_REFERENCE_CLIENT_SECRET")
		if referenceEndpoint == "" || referenceTokenURL == "" || referenceClientID == "" || referenceSecret == "" {
			slog.Warn("project reference sync disabled: SETTLEMENT_PROJECT_REFERENCE_* machine credentials are incomplete")
		} else {
			resolver, resolverErr := projectreference.New(referenceEndpoint, referenceClientID, referenceSecret, referenceTokenURL, 8*time.Second)
			if resolverErr != nil {
				slog.Error("project reference sync client configuration failed", "error", resolverErr)
				os.Exit(1)
			}
			projectReferenceSync = &service.ProjectReferenceSyncWorker{DB: db, Resolver: resolver, BatchSize: projectReferenceSyncBatch, CheckBusinessLicense: checkBusiness, Logf: func(format string, args ...any) {
				slog.Warn(fmt.Sprintf(format, args...), "task", "project_reference_sync")
			}}
		}
	}
	exporter, exportErr := newReportExporter(db, workerID)
	if exportErr != nil {
		slog.Error("report export configuration failed", "error", exportErr)
		os.Exit(1)
	}
	if exporter.GatewayMode == "dual" || exporter.GatewayMode == "required" {
		endpoint, clientID, secret := strings.TrimRight(strings.TrimSpace(os.Getenv("FILE_GATEWAY_URL")), "/"), os.Getenv("FILE_GATEWAY_CLIENT_ID"), os.Getenv("FILE_GATEWAY_CLIENT_SECRET")
		tokenURL, scope := os.Getenv("FILE_GATEWAY_TOKEN_URL"), os.Getenv("FILE_GATEWAY_SCOPE")
		if tokenURL == "" {
			tokenURL = platform + "/oauth2/token"
		}
		tokenSource := func(ctx context.Context) (string, error) {
			return requestFileGatewayToken(ctx, httpClient, tokenURL, clientID, secret, scope)
		}
		gateway, gatewayErr := filegatewayclient.New(endpoint, httpClient, tokenSource)
		if gatewayErr != nil {
			slog.Error("file gateway configuration failed", "error", gatewayErr)
			if exporter.GatewayMode == "required" {
				os.Exit(1)
			}
		} else {
			exporter.Gateway = gateway
		}
	}
	// Recovery uses the same tenant-bound machine identity as report exports.
	// A missing gateway leaves durable work pending and is reported each run.
	archiveRecovery := &service.InvoiceArchiveWorker{DB: db, Identity: service.InvoiceArchiveIdentity{
		TenantID: exporter.TenantID, ApplicationID: exporter.GatewayApplicationID,
		GatewayURL: strings.TrimRight(strings.TrimSpace(os.Getenv("FILE_GATEWAY_URL")), "/"),
	}, CheckBusinessLicense: checkBusiness}
	if exporter.Gateway != nil {
		archiveRecovery.Gateway = exporter.Gateway
	}
	receiptRecovery := &service.ReceiptArchiveWorker{DB: db, Identity: archiveRecovery.Identity, Gateway: archiveRecovery.Gateway, CheckBusinessLicense: checkBusiness}
	if exporter.GatewayMode == "dual" || exporter.GatewayMode == "required" {
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				if err := receiptRecovery.RunOnce(ctx); err != nil && ctx.Err() == nil {
					slog.Error("receipt archive recovery failed", "error", err)
				}
				if err := archiveRecovery.RunOnce(ctx); err != nil && ctx.Err() == nil {
					slog.Error("invoice archive recovery failed", "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if err := roleReminder.RunOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Error("contract owner reminders failed", "error", err)
			}
			if err := scanner.RunOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Error("dunning scan failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	if taxReconciler != nil {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				if err := taxReconciler.RunOnce(ctx); err != nil && ctx.Err() == nil {
					slog.Error("tax result reconciliation failed", "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := exporter.RunOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Error("report export failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	if creditWorker != nil {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				if err := creditWorker.RunOnce(ctx); err != nil && ctx.Err() == nil {
					slog.Error("CRM credit retry failed", "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if projectReferenceSync != nil {
		go func() {
			ticker := time.NewTicker(projectReferenceSyncInterval)
			defer ticker.Stop()
			for {
				if err := projectReferenceSync.RunOnce(ctx); err != nil && ctx.Err() == nil {
					slog.Error("project reference sync failed", "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if err = worker.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("settlement worker stopped", "error", err)
		os.Exit(1)
	}
}
