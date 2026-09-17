package main

import (
	"context"
	"database/sql"
	"fmt"
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
	"github.com/j-s-te/settlement/internal/httpapi"
	"github.com/j-s-te/settlement/internal/platform"
	"golang.org/x/oauth2/clientcredentials"
)

func main() {
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
			machine, err = platform.NewServiceTokenVerifier(context.Background(), cfg.OIDCIssuer, cfg.ContractClientID, cfg.ContractAudience, cfg.OIDCTenantID, "contract_management", cfg.OIDCEnvironmentCode, "settlement.contract.ingest")
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
