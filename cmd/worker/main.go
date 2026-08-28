package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/dunning"
	"github.com/j-s-te/settlement/internal/filegatewayclient"
	"github.com/j-s-te/settlement/internal/outbox"
	"github.com/j-s-te/settlement/internal/reportexport"
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
	if id, secret := os.Getenv("SETTLEMENT_NOTIFICATION_CLIENT_ID"), os.Getenv("SETTLEMENT_NOTIFICATION_CLIENT_SECRET"); id != "" && secret != "" {
		destinations = append(destinations, outbox.Destination{Name: "PLATFORM_NOTIFICATION", Endpoint: platform + "/api/v1/notifications/events/batch", Scope: "notification.ingest", ClientID: id, ClientSecret: secret, ApplicationCode: "settlement", EnvironmentCode: environment})
	}
	if id, secret := os.Getenv("SETTLEMENT_AUDIT_CLIENT_ID"), os.Getenv("SETTLEMENT_AUDIT_CLIENT_SECRET"); id != "" && secret != "" {
		destinations = append(destinations, outbox.Destination{Name: "PLATFORM_AUDIT", Endpoint: platform + "/api/v1/audit/events/batch", Scope: "audit.ingest", ClientID: id, ClientSecret: secret, ApplicationCode: "settlement", EnvironmentCode: environment})
	}
	if endpoint, id, secret := strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_ADAPTER_URL")), os.Getenv("SETTLEMENT_TAX_CLIENT_ID"), os.Getenv("SETTLEMENT_TAX_CLIENT_SECRET"); endpoint != "" && id != "" && secret != "" {
		destinations = append(destinations, outbox.Destination{Name: "TAX_INVOICE_COMMAND", Endpoint: endpoint, TokenEndpoint: strings.TrimSpace(os.Getenv("SETTLEMENT_TAX_TOKEN_URL")), Scope: "invoice.issue", ClientID: id, ClientSecret: secret, ApplicationCode: "settlement", EnvironmentCode: environment})
	}
	httpClient := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	worker := &outbox.Worker{Store: &outbox.Store{DB: db, WorkerID: workerID, Lease: 30 * time.Second}, HTTP: httpClient, TokenEndpoint: platform + "/oauth2/token", BatchSize: 50, Destinations: destinations}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	scanner := &dunning.Scanner{DB: db, BatchSize: 50}
	exporter := &reportexport.Worker{DB: db, WorkerID: workerID, BatchSize: 5, GatewayMode: strings.ToLower(strings.TrimSpace(os.Getenv("SETTLEMENT_FILE_GATEWAY_MODE"))), GatewayApplicationID: strings.TrimSpace(os.Getenv("SETTLEMENT_FILE_GATEWAY_APPLICATION_ID"))}
	exporter.GatewayMode = reportexport.NormalizeGatewayMode(exporter.GatewayMode)
	if exporter.GatewayMode == "dual" || exporter.GatewayMode == "required" {
		endpoint, clientID, secret := strings.TrimRight(strings.TrimSpace(os.Getenv("FILE_GATEWAY_URL")), "/"), os.Getenv("FILE_GATEWAY_CLIENT_ID"), os.Getenv("FILE_GATEWAY_CLIENT_SECRET")
		tokenURL, scope := os.Getenv("FILE_GATEWAY_TOKEN_URL"), os.Getenv("FILE_GATEWAY_SCOPE")
		if tokenURL == "" {
			tokenURL = platform + "/oauth2/token"
		}
		tokenSource := func(ctx context.Context) (string, error) {
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader("grant_type=client_credentials&client_id="+url.QueryEscape(clientID)+"&client_secret="+url.QueryEscape(secret)+"&scope="+url.QueryEscape(scope)))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response, err := httpClient.Do(request)
			if err != nil {
				return "", err
			}
			defer response.Body.Close()
			var payload struct {
				AccessToken string `json:"access_token"`
			}
			if err = json.NewDecoder(response.Body).Decode(&payload); err != nil || response.StatusCode/100 != 2 || payload.AccessToken == "" {
				return "", fmt.Errorf("file gateway token request failed: %s", response.Status)
			}
			return payload.AccessToken, nil
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
	if err = worker.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("settlement worker stopped", "error", err)
		os.Exit(1)
	}
}
