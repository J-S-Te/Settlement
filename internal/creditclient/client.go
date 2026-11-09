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

// DeliveryError 表示 CRM 信用入口返回了非 2xx 响应。
// 401/403 是凭据或用途配置错误，不是瞬时故障：调用方必须能把它们与
// 5xx/网络错误分开，否则运营会把接入配置漂移当成可重试的投递失败反复重试。
type DeliveryError struct {
	StatusCode int
}

func (e *DeliveryError) Error() string {
	return fmt.Sprintf("CRM credit endpoint returned HTTP %d", e.StatusCode)
}

// AuthorizationDenied 判断信用投递失败是否属于凭据/用途授权错误。
// 覆盖 CRM 业务入口的 401/403，以及平台令牌端点的 invalid_client /
// invalid_scope / unauthorized_client（平台没有该用途时正落在这里）。
// 这些错误只能通过修复受控接入配置解决，重试不会自愈。
func AuthorizationDenied(err error) bool {
	var delivery *DeliveryError
	if errors.As(err, &delivery) {
		return delivery.StatusCode == http.StatusUnauthorized || delivery.StatusCode == http.StatusForbidden
	}
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		switch retrieve.ErrorCode {
		case "invalid_client", "invalid_scope", "unauthorized_client":
			return true
		}
		if retrieve.Response != nil && (retrieve.Response.StatusCode == http.StatusUnauthorized || retrieve.Response.StatusCode == http.StatusForbidden) {
			return true
		}
	}
	return false
}

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

// Publish 将持久化信用事件按CRM契约投递至已配置入口。
// 参数：ctx为请求上下文，tenant为可信租户，eventID为追踪标识，payload为持久化报文。
// 返回值：规范化、许可、网络或CRM拒绝错误；成功返回nil。
func (c *Client) Publish(ctx context.Context, tenant, eventID string, payload []byte) error {
	if err := c.checkLicense(ctx); err != nil {
		return err
	}
	wire, err := normalizePaymentWire(tenant, payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(wire))
	if err != nil {
		return err
	}
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate CRM integration nonce: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", eventID)
	// tenant 仅校验持久化报文归属，不出站传递：接收方租户从已验签机器令牌推导，
	// 请求头中的租户标识不会被任何接收方读取，携带反而会诱导信任未验签输入。
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
		return &DeliveryError{StatusCode: resp.StatusCode}
	}
	return nil
}
