package creditclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

var ErrLicensePaused = errors.New("commercial license paused CRM credit delivery")

type Client struct {
	endpoint             string
	client               *http.Client
	checkBusinessLicense func(context.Context) error
}

func New(baseURL, tokenURL, clientID, clientSecret, scope string, httpClient *http.Client) (*Client, error) {
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("CRM credit endpoint must be HTTP(S)")
	}
	if tokenURL == "" || clientID == "" || clientSecret == "" || scope == "" {
		return nil, fmt.Errorf("CRM credit OAuth configuration is incomplete")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	tokenConfig := &clientcredentials.Config{ClientID: clientID, ClientSecret: clientSecret, TokenURL: tokenURL, Scopes: []string{scope}}
	c := &Client{endpoint: strings.TrimRight(baseURL, "/")}
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	// OAuth 在调用 Base.RoundTrip 前获取或刷新令牌。将
	// 业务检查放在 Base 内，防止令牌延迟后复用较早门禁结果。
	tokenContext := context.WithValue(context.Background(), oauth2.HTTPClient, httpClient)
	client := *httpClient
	client.Transport = &oauth2.Transport{Source: tokenConfig.TokenSource(tokenContext), Base: &licensedTransport{client: c, base: base}}
	c.client = &client
	return c, nil
}

// SetBusinessLicenseCheck 必须在启动期间、发布前调用。
// nil 检查保留既有不受门禁约束客户端调用方的兼容性。
func (c *Client) SetBusinessLicenseCheck(check func(context.Context) error) {
	c.checkBusinessLicense = check
}

func (c *Client) checkLicense(ctx context.Context) error {
	if c.checkBusinessLicense != nil {
		if err := c.checkBusinessLicense(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrLicensePaused, err)
		}
	}
	return nil
}

type licensedTransport struct {
	client *Client
	base   http.RoundTripper
}

func (t *licensedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.client.checkLicense(req.Context()); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func (c *Client) Publish(ctx context.Context, tenant, eventID string, payload []byte) error {
	if err := c.checkLicense(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate CRM integration nonce: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", eventID)
	req.Header.Set("X-Tenant-ID", tenant)
	// CRM 的机器入口要求时间戳 + 一次性 nonce；只带 Bearer 令牌会在验签后被拒绝。
	req.Header.Set("X-Integration-Timestamp", time.Now().UTC().Format(time.RFC3339Nano))
	req.Header.Set("X-Integration-Nonce", base64.RawURLEncoding.EncodeToString(nonce))
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("CRM credit endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}
