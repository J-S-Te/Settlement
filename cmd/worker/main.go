package main

import (
	"context"
	"database/sql"
	"github.com/J-S-Te/license-core/consumer"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/creditclient"
	"github.com/j-s-te/settlement/internal/dunning"
	"github.com/j-s-te/settlement/internal/filegatewayclient"
	"github.com/j-s-te/settlement/internal/outbox"
	platformdir "github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/internal/taxreconcile"
	"golang.org/x/oauth2/clientcredentials"
)

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
	var taxReconciler *taxreconcile.Worker
	issuanceMode := strings.ToLower(strings.TrimSpace(os.Getenv("SETTLEMENT_INVOICE_ISSUANCE_MODE")))
	if issuanceMode == "" {
		issuanceMode = "manual"
	}
	if issuanceMode != "manual" && issuanceMode != "tax_adapter" {
		slog.Error("SETTLEMENT_INVOICE_ISSUANCE_MODE must be manual or tax_adapter")
		os.Exit(1)
	}
	if id, secret := os.Getenv("SETTLEMENT_NOTIFICATION_CLIENT_ID"), os.Getenv("SETTLEMENT_NOTIFICATION_CLIENT_SECRET"); id != "" && secret != "" {
		destinations = append(destinations, outbox.Destination{Name: "PLATFORM_NOTIFICATION", Endpoint: platform + "/api/v1/notifications/events/batch", Scope: "notification.ingest", ClientID: id, ClientSecret: secret, ApplicationCode: "settlement", EnvironmentCode: environment})
	}
	if id, secret := os.Getenv("SETTLEMENT_AUDIT_CLIENT_ID"), os.Getenv("SETTLEMENT_AUDIT_CLIENT_SECRET"); id != "" && secret != "" {
		destinations = append(destinations, outbox.Destination{Name: "PLATFORM_AUDIT", Endpoint: platform + "/api/v1/audit/events/batch", Scope: "audit.ingest", ClientID: id, ClientSecret: secret, ApplicationCode: "settlement", EnvironmentCode: environment})
	}
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
		scope := strings.TrimSpace(os.Getenv("CRM_CREDIT_SCOPE"))
		if scope == "" {
			scope = "crm.credit.payment.ingest"
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
	worker := &outbox.Worker{Store: &outbox.Store{DB: db, WorkerID: workerID, Lease: 30 * time.Second}, HTTP: httpClient, TokenEndpoint: platform + "/oauth2/token", BatchSize: 50, Destinations: destinations}
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
	if exporter.GatewayMode == "dual" || exporter.GatewayMode == "required" {
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
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
	if err = worker.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("settlement worker stopped", "error", err)
		os.Exit(1)
	}
}
