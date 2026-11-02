// Package sessiongovernance 封装基础平台中央会话协议，不承载业务授权。
package sessiongovernance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrInactive        = errors.New("central session is inactive")
	ErrUnavailable     = errors.New("central session service is unavailable")
	ErrBindingConflict = errors.New("central session binding conflicts")
)

// Context 是平台返回的中央生命周期，绝对截止不由RP重新起算。
type Context struct {
	SessionID           string    `json:"session_id"`
	TenantID            string    `json:"tenant_id"`
	SubjectID           string    `json:"subject_id"`
	ClientID            string    `json:"client_id"`
	ApplicationCode     string    `json:"application_code"`
	EnvironmentCode     string    `json:"environment_code"`
	PolicyVersion       uint64    `json:"policy_version"`
	IdleTimeoutSeconds  int64     `json:"idle_timeout_seconds"`
	IdleRemainingMS     int64     `json:"idle_remaining_ms"`
	AbsoluteExpiresAt   time.Time `json:"absolute_expires_at"`
	AbsoluteRemainingMS int64     `json:"absolute_remaining_ms"`
}

// Binding 只在服务器端返回并加密持久化，禁止向浏览器传递。
type Binding struct {
	Token   string  `json:"binding_token"`
	Context Context `json:"session_context"`
}

// Client 是登录桥接、状态读取与真实交互代理的统一边界。
type Client interface {
	BeginURL(state, nonce string) string
	Complete(context.Context, string, string, string) (Binding, error)
	State(context.Context, string) (Context, error)
	Activity(context.Context, string) (Context, error)
}

// Expectation 保留应用、环境和租户的本地信任边界。
type Expectation struct{ TenantID, ClientID, ApplicationCode, EnvironmentCode string }

// HTTPClient 分开浏览器公开入口和服务器内网入口，不重写任意URL。
type HTTPClient struct {
	base, begin, clientID, redirect string
	http                            *http.Client
}

// NewHTTPClient 用途：构造仅访问已配置平台入口的中央协议客户端。
// 参数：base为内网API，public为浏览器入口，clientID与redirect为已登记RP身份及回调。
// 返回值：客户端；地址或身份不完整时返回配置错误。
func NewHTTPClient(base, public, clientID, redirect string) (*HTTPClient, error) {
	for _, raw := range []string{base, public, redirect} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("invalid central session endpoint configuration")
		}
	}
	if strings.TrimSpace(clientID) == "" {
		return nil, errors.New("central session client is missing")
	}
	return &HTTPClient{base: strings.TrimRight(base, "/"), begin: strings.TrimRight(public, "/") + "/oauth2/session-links/begin", clientID: clientID, redirect: redirect, http: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// BeginURL 用途：把现有state与nonce摘要绑定到平台一次性桥接。
// 参数：state为RP原始随机状态，nonce为OIDC原始随机nonce。
// 返回值：不含nonce原文及任何会话秘密的公开平台URL。
func (c *HTTPClient) BeginURL(state, nonce string) string {
	sum := sha256.Sum256([]byte(nonce))
	q := url.Values{"client_id": {c.clientID}, "redirect_uri": {c.redirect}, "state": {state}, "nonce_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	return c.begin + "?" + q.Encode()
}

// Complete 用途：提交已验证OIDC材料并原子消费平台桥接票据。
// 参数：ctx为请求上下文，ticket为桥接票据，access与id为提供方签发的令牌。
// 返回值：中央绑定和权威窗口；拒绝、冲突与暂不可用分别返回类型错误。
func (c *HTTPClient) Complete(ctx context.Context, ticket, access, id string) (Binding, error) {
	var result Binding
	if ticket == "" || access == "" || id == "" {
		return result, ErrInactive
	}
	err := c.request(ctx, http.MethodPost, "/oauth2/session-links/complete", "Bearer "+access, map[string]string{"ticket": ticket, "id_token": id}, &result)
	if err == nil && result.Token == "" {
		err = ErrUnavailable
	}
	return result, err
}

// State 用途：只读查询中央生命周期，不推进任何期限。
// 参数：ctx为请求上下文，binding为服务器端解密的中央绑定秘密。
// 返回值：权威窗口；失效返回ErrInactive，网络或平台故障返回ErrUnavailable。
func (c *HTTPClient) State(ctx context.Context, binding string) (Context, error) {
	var result Context
	err := c.request(ctx, http.MethodGet, "/oauth2/session-context", "SessionBinding "+binding, nil, &result)
	return result, err
}

// Activity 用途：代理同源已认证页面的真实交互到中央会话。
// 参数：ctx为请求上下文，binding为服务器端解密的中央绑定秘密。
// 返回值：续期后的权威窗口；已失效会话不可复活。
func (c *HTTPClient) Activity(ctx context.Context, binding string) (Context, error) {
	var result Context
	err := c.request(ctx, http.MethodPost, "/oauth2/session/activity", "SessionBinding "+binding, nil, &result)
	return result, err
}

// Validate 用途：核验中央响应没有跨越当前应用、环境、租户或主体。
// 参数：value为权威响应，expected为本地配置，subject为已认证主体。
// 返回值：可信有效时nil；绑定错位或明确到期返回ErrInactive。
func Validate(value Context, expected Expectation, subject string) error {
	if value.IdleTimeoutSeconds > 1800 || value.IdleRemainingMS > value.IdleTimeoutSeconds*1000 ||
		value.AbsoluteRemainingMS > (8*time.Hour).Milliseconds() || value.IdleRemainingMS > value.AbsoluteRemainingMS {
		return ErrUnavailable
	}
	if value.SessionID == "" || value.TenantID != expected.TenantID || value.SubjectID != subject || value.ClientID != expected.ClientID || value.ApplicationCode != expected.ApplicationCode || value.EnvironmentCode != expected.EnvironmentCode || value.PolicyVersion == 0 || value.IdleTimeoutSeconds <= 0 || value.IdleRemainingMS <= 0 || value.AbsoluteRemainingMS <= 0 || value.AbsoluteExpiresAt.IsZero() {
		return ErrInactive
	}
	return nil
}

// request 用途：发送有界中央协议请求且不把响应正文或秘密写入错误。
// 参数：ctx为上下文，method/path为固定协议端点，authorization为绑定认证，body为JSON体。
// 返回值：解码结果写入target；错误按失效、冲突及依赖不可用分类。
func (c *HTTPClient) request(ctx context.Context, method, path, authorization string, body any, target any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return ErrUnavailable
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return ErrInactive
	case http.StatusConflict:
		return ErrBindingConflict
	case http.StatusOK:
	default:
		return ErrUnavailable
	}
	mediaType, _, typeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if typeErr != nil || mediaType != "application/json" {
		return ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return ErrUnavailable
	}
	if err = json.Unmarshal(raw, target); err != nil {
		return ErrUnavailable
	}
	return nil
}
