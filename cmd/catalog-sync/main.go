package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/j-s-te/settlement/authz"
	"github.com/j-s-te/settlement/internal/catalogsync"
)

func main() {
	cfg := catalogsync.Config{
		PlatformBaseURL: strings.TrimSpace(os.Getenv("PLATFORM_BASE_URL")),
		ApplicationID:   strings.TrimSpace(os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID")),
		ClientID:        strings.TrimSpace(os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID")),
		ClientSecret:    os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	if err := catalogsync.Publish(ctx, client, cfg, authz.PermissionManifest); err != nil {
		slog.Error("authorization catalog synchronization failed", "error", err)
		os.Exit(1)
	}
	slog.Info("authorization catalog synchronized")
}
