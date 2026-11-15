package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/j-s-te/settlement/internal/outbox"
)

// reportOutboxHealth 周期上报结算 outbox 积压；dead 暂时没有独立终态，按 0 上报。
func reportOutboxHealth(ctx context.Context, db *sql.DB, platform, environment string, logger *slog.Logger) {
	clientID := strings.TrimSpace(os.Getenv("SETTLEMENT_AUDIT_CLIENT_ID"))
	clientSecret := os.Getenv("SETTLEMENT_AUDIT_CLIENT_SECRET")
	if db == nil || strings.TrimSpace(platform) == "" || clientID == "" || clientSecret == "" {
		return
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		store := &outbox.Store{DB: db}
		pending := int64(0)
		if counts, err := store.PendingByDestination(ctx); err == nil {
			for _, count := range counts {
				pending += count
			}
		}
		tokenForm := url.Values{"grant_type": {"client_credentials"}, "scope": {"audit.ingest"}}
		tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(platform, "/")+"/oauth2/token", strings.NewReader(tokenForm.Encode()))
		if err == nil {
			tokenReq.SetBasicAuth(clientID, clientSecret)
			tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, tokenErr := (&http.Client{Timeout: 5 * time.Second}).Do(tokenReq)
			if tokenErr == nil {
				var payload map[string]any
				_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload)
				resp.Body.Close()
				token, _ := payload["access_token"].(string)
				if token != "" {
					body, _ := json.Marshal(map[string]any{"pending": pending, "dead": 0})
					req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(platform, "/")+"/api/v1/audit/outbox-health", bytes.NewReader(body))
					req.Header.Set("Authorization", "Bearer "+token)
					req.Header.Set("Content-Type", "application/json")
					if reportResp, reportErr := (&http.Client{Timeout: 5 * time.Second}).Do(req); reportErr == nil {
						io.Copy(io.Discard, io.LimitReader(reportResp.Body, 1<<20))
						reportResp.Body.Close()
					} else {
						logger.Warn("report settlement outbox health failed", "error", reportErr)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
