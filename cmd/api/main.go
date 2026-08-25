package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/httpapi"
	"github.com/j-s-te/settlement/internal/platform"
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
	}
	server := &http.Server{Addr: cfg.HTTPAddress, Handler: httpapi.New(db, cfg, slog.Default(), auth, machine, personnelDirectory), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
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
