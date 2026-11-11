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
	"github.com/j-s-te/settlement/internal/filegatewayclient"
	"github.com/j-s-te/settlement/internal/handover"
	"github.com/j-s-te/settlement/internal/httpapi"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/projectreference"
	"golang.org/x/oauth2/clientcredentials"
)

// main 装配结算API及独立机器用途，启动受控HTTP服务。
// 参数：无，读取已校验运行配置。
// 返回值：无，初始化失败时退出进程。
func main() {
	licenseCtx, licenseStop := context.WithCancel(context.Background())
	defer licenseStop()
	commercialGate, licenseErr := consumer.FromEnvironment("settlement")
	if licenseErr != nil {
		slog.Error("commercial license configuration failed")
		os.Exit(1)
	}
	go func() {
		_ = commercialGate.Run(licenseCtx, func(error) {
			slog.Warn("commercial license synchronization unavailable; local expiry remains authoritative")
		})
	}()
	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		slog.Error("ping database", "error", err)
		os.Exit(1)
	}
	var auth *platform.Authenticator
	var machine *platform.ServiceTokenVerifier
	var taxMachine *platform.ServiceTokenVerifier
	var personnelDirectory platform.PersonnelDirectory
	personnelDirectoryURL := cfg.PersonnelDirectoryURL
	if personnelDirectoryURL == "" {
		personnelDirectoryURL = cfg.PlatformBaseURL
	}
	personnelDirectory = platform.NewPersonnelDirectory(personnelDirectoryURL, cfg.PersonnelDirectoryClientID, cfg.PersonnelDirectoryClientSecret, 10*time.Second)
	if !cfg.DevelopmentAuth {
		auth, err = platform.NewAuthenticator(context.Background(), db, cfg)
		if err != nil {
			slog.Error("initialize OIDC", "error", err)
			os.Exit(1)
		}
		if cfg.IntegrationEnabled {
			machine, err = platform.NewContractServiceTokenVerifier(context.Background(), cfg)
			if err != nil {
				slog.Error("initialize machine token verifier", "error", err)
				os.Exit(1)
			}
		}
		if cfg.TaxResultIngestEnabled {
			taxMachine, err = platform.NewStrictServiceTokenVerifier(context.Background(), cfg.OIDCIssuer, cfg.TaxResultClientID, cfg.TaxResultAudience, cfg.OIDCTenantID, cfg.TaxResultApplicationCode, cfg.OIDCEnvironmentCode, "settlement.tax_result.ingest")
			if err != nil {
				slog.Error("initialize tax result token verifier", "error", err)
				os.Exit(1)
			}
		}
	}
	var creditPublisher *creditclient.Client
	if cfg.CRMCreditSyncEnabled {
		creditPublisher, err = creditclient.New(cfg.CRMCreditEndpoint, cfg.CRMCreditTokenEndpoint, cfg.CRMCreditClientID, cfg.CRMCreditClientSecret, cfg.CRMCreditScope, &http.Client{Timeout: 8 * time.Second})
		if err != nil {
			slog.Error("initialize CRM credit client", "error", err)
			os.Exit(1)
		}
		creditPublisher.SetBusinessLicenseCheck(func(ctx context.Context) error { return commercialGate.Check(ctx, "MUTATE_BUSINESS") })
	}
	var fileGateway *filegatewayclient.Client
	if (cfg.FileGatewayMode == "dual" || cfg.FileGatewayMode == "required") && cfg.FileGatewayURL != "" {
		if cfg.FileGatewayClientID == "" || cfg.FileGatewayClientSecret == "" || cfg.FileGatewayTokenURL == "" || cfg.FileGatewayApplicationID == "" {
			err = fmt.Errorf("file gateway API credentials and application ID are incomplete")
		} else {
			httpClient := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
			tokenConfig := &clientcredentials.Config{ClientID: cfg.FileGatewayClientID, ClientSecret: cfg.FileGatewayClientSecret, TokenURL: cfg.FileGatewayTokenURL, Scopes: strings.Fields(cfg.FileGatewayScope)}
			fileGateway, err = filegatewayclient.New(cfg.FileGatewayURL, httpClient, func(ctx context.Context) (string, error) {
				token, tokenErr := tokenConfig.Token(ctx)
				if tokenErr != nil {
					return "", tokenErr
				}
				return token.AccessToken, nil
			})
		}
		if err != nil {
			if cfg.FileGatewayMode == "required" {
				slog.Error("initialize file gateway", "error", err)
				os.Exit(1)
			}
			slog.Warn("file gateway disabled after configuration error", "error", err)
			fileGateway = nil
		}
	}
	dependencies := []any{personnelDirectory}
	if creditPublisher != nil {
		dependencies = append(dependencies, creditPublisher)
	}
	if fileGateway != nil {
		dependencies = append(dependencies, fileGateway)
	}
	dependencies = append(dependencies, commercialGate)
	if cfg.DashboardMachineEnabled {
		dashboardMachine, verifyErr := platform.NewDashboardTokenVerifier(context.Background(), platform.ClientCredentialsVerifierOptions{
			Issuer: cfg.DashboardMachineIssuer, Audience: cfg.DashboardMachineAudience,
			PublicKeyPath: cfg.DashboardMachinePublicKeyPath, ClientID: cfg.DashboardMachineClientID,
			TenantID: cfg.OIDCTenantID, CallerApplicationCode: cfg.DashboardMachineCallerApplicationCode,
			CallerEnvironmentCode: cfg.DashboardMachineCallerEnvironmentCode, RequiredScope: cfg.DashboardMachineRequiredScope,
		}, cfg.DashboardMachineOAuthClientID)
		if verifyErr != nil {
			slog.Error("initialize dashboard machine token verifier", "error", verifyErr)
			os.Exit(1)
		}
		dependencies = append(dependencies, &httpapi.DashboardIntegrationOptions{Verifier: dashboardMachine})
	}
	if cfg.HandoverMachineEnabled {
		// 交接协议与合同/项目仓库使用同一平台 application JWT 信任域：
		// 平台自身 ApplicationJWTManager 签发（caller=platform、env=prod、scope=handover.machine），
		// 租户来自签名 claims；不再走 OIDC 严格验签器。
		handoverMachine, verifyErr := platform.NewClientCredentialsTokenVerifier(context.Background(), platform.ClientCredentialsVerifierOptions{
			Issuer: cfg.HandoverMachineIssuer, Audience: cfg.HandoverMachineAudience,
			PublicKeyPath: cfg.HandoverMachinePublicKeyPath, ClientID: cfg.HandoverMachineClientID,
			TenantID: cfg.OIDCTenantID, CallerApplicationCode: cfg.HandoverMachineApplicationCode,
			CallerEnvironmentCode: cfg.HandoverMachineCallerEnvironmentCode, RequiredScope: cfg.HandoverMachineScope,
		})
		if verifyErr != nil {
			slog.Error("initialize handover machine token verifier", "error", verifyErr)
			os.Exit(1)
		}
		handoverRepository, repositoryErr := handover.NewRepository(db)
		if repositoryErr != nil {
			slog.Error("initialize handover repository", "error", repositoryErr)
			os.Exit(1)
		}
		handoverService, serviceErr := handover.NewService(handoverRepository, handover.NewID, time.Now)
		if serviceErr != nil {
			slog.Error("initialize handover service", "error", serviceErr)
			os.Exit(1)
		}
		dependencies = append(dependencies, handoverService, handoverMachine)
	}
	if cfg.ProjectReferenceURL != "" {
		projectClient, clientErr := projectreference.New(cfg.ProjectReferenceURL, cfg.ProjectReferenceClientID, cfg.ProjectReferenceClientSecret, cfg.ProjectReferenceTokenURL, 10*time.Second)
		if clientErr != nil {
			slog.Error("initialize project reference client", "error", clientErr)
			os.Exit(1)
		}
		dependencies = append(dependencies, projectClient)
	}
	server := &http.Server{Addr: cfg.HTTPAddress, Handler: httpapi.New(db, cfg, slog.Default(), auth, machine, taxMachine, dependencies...), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	slog.Info("settlement API started", "address", cfg.HTTPAddress, "development_auth", cfg.DevelopmentAuth)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			slog.Error("server shutdown", "error", err)
		}
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
