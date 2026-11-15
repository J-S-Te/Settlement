package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// reportSecurityEvent 把未认证写请求上报平台全局 security_event。
// 认证失败没有可信租户，不能写租户审计；这里使用审计机器凭据，由平台按令牌归属应用。
func (a *API) reportSecurityEvent(r *http.Request, requestID string, status int) {
	if a == nil || a.cfg.PlatformBaseURL == "" || a.cfg.AuditClientID == "" || a.cfg.AuditClientSecret == "" {
		if a != nil && a.logger != nil {
			a.logger.Warn("settlement security event skipped: audit client is not configured", "path", r.URL.Path)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	token, err := a.securityEventToken(ctx)
	if err != nil {
		a.logger.Error("settlement security event token failed", "error", err)
		return
	}
	result := "SUCCESS"
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		result = "DENIED"
	} else if status >= http.StatusBadRequest {
		result = "FAILURE"
	}
	clientIP, _ := a.clientIP(r)
	payload := map[string]any{
		"event_id": randomSecurityID(), "occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"action": "AUTHENTICATION_DENIED", "method": r.Method, "path": r.URL.Path,
		"client_ip": clientIP, "user_agent": r.UserAgent(), "request_id": requestID,
		"result": result, "reason_code": fmt.Sprintf("%d", status), "risk_level": "HIGH",
		"summary": "settlement unauthenticated API request",
	}
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.PlatformBaseURL, "/")+"/api/v1/security-events", bytes.NewReader(body))
	if err != nil {
		a.logger.Error("settlement security event request failed", "error", err)
		return
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", requestID)
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		a.logger.Error("settlement security event post failed", "error", err)
		return
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK {
		a.logger.Error("settlement security event rejected", "status", response.StatusCode)
	}
}

func (a *API) securityEventToken(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"audit.ingest"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.PlatformBaseURL, "/")+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.SetBasicAuth(a.cfg.AuditClientID, a.cfg.AuditClientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return "", fmt.Errorf("platform token returned %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	accessToken, _ := payload["access_token"].(string)
	tokenType, _ := payload["token_type"].(string)
	scope, _ := payload["scope"].(string)
	if accessToken == "" || !strings.EqualFold(tokenType, "bearer") || !containsScope(scope, "audit.ingest") {
		return "", fmt.Errorf("platform token missing audit.ingest scope")
	}
	return accessToken, nil
}

func containsScope(value, wanted string) bool {
	for _, scope := range strings.Fields(value) {
		if scope == wanted {
			return true
		}
	}
	return false
}

func randomSecurityID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
