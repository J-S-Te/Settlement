package catalogsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Config struct {
	PlatformBaseURL string
	ApplicationID   string
	ClientID        string
	ClientSecret    string
}

func Publish(ctx context.Context, client *http.Client, cfg Config, manifest []byte) error {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.PlatformBaseURL), "/")
	if baseURL == "" || strings.TrimSpace(cfg.ApplicationID) == "" || strings.TrimSpace(cfg.ClientID) == "" || cfg.ClientSecret == "" || len(manifest) == 0 {
		return errors.New("catalog publisher configuration is incomplete")
	}
	if client == nil {
		return errors.New("catalog publisher HTTP client is required")
	}

	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"authorization.catalog.sync"}}
	tokenRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("create catalog token request: %w", err)
	}
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRequest.SetBasicAuth(cfg.ClientID, cfg.ClientSecret)
	tokenResponse, err := client.Do(tokenRequest)
	if err != nil {
		return fmt.Errorf("request catalog publisher token: %w", err)
	}
	defer tokenResponse.Body.Close()
	if tokenResponse.StatusCode < http.StatusOK || tokenResponse.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("catalog publisher token request returned status %d", tokenResponse.StatusCode)
	}
	var tokenPayload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(tokenResponse.Body, 1<<20)).Decode(&tokenPayload); err != nil || strings.TrimSpace(tokenPayload.AccessToken) == "" {
		return errors.New("catalog publisher token response is invalid")
	}

	catalogRequest, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/api/v1/applications/"+url.PathEscape(cfg.ApplicationID)+"/authorization-catalog", bytes.NewReader(manifest))
	if err != nil {
		return fmt.Errorf("create catalog publication request: %w", err)
	}
	catalogRequest.Header.Set("Authorization", "Bearer "+tokenPayload.AccessToken)
	catalogRequest.Header.Set("Content-Type", "application/json")
	catalogResponse, err := client.Do(catalogRequest)
	if err != nil {
		return fmt.Errorf("publish authorization catalog: %w", err)
	}
	defer catalogResponse.Body.Close()
	if catalogResponse.StatusCode < http.StatusOK || catalogResponse.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("authorization catalog publication returned status %d", catalogResponse.StatusCode)
	}
	var catalogPayload struct {
		Data struct {
			SyncStatus string `json:"sync_status"`
		} `json:"data"`
		SyncStatus string `json:"sync_status"`
	}
	if err := json.NewDecoder(io.LimitReader(catalogResponse.Body, 1<<20)).Decode(&catalogPayload); err != nil {
		return errors.New("authorization catalog response is invalid")
	}
	if catalogPayload.Data.SyncStatus != "SYNCED" && catalogPayload.SyncStatus != "SYNCED" {
		return errors.New("platform did not confirm authorization catalog synchronization")
	}
	return nil
}
