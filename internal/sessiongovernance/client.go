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
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrInactive        = errors.New("central session is inactive")
	ErrUnavailable     = errors.New("central session service is unavailable")
	ErrBindingConflict = errors.New("central session binding conflicts")
)

const (
	stateTimeout    = 800 * time.Millisecond
	activityTimeout = 2 * time.Second
	completeTimeout = 5 * time.Second

	stateCacheEnv       = "CENTRAL_SESSION_CACHE_MS"
	stateCacheDefaultMS = 2000
	stateCacheMaxMS     = 5000
	stateCacheMaxItems  = 4096
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

// stateCacheEntry 只保存已验证的权威窗口和写入时刻，键为binding摘要。
type stateCacheEntry struct {
	value Context
	at    time.Time
}

// HTTPClient 分开浏览器公开入口和服务器内网入口，不重写任意URL。
type HTTPClient struct {
	base, begin, clientID, redirect string
	http                            *http.Client
	cacheMu                         sync.Mutex
	cache                           map[[32]byte]stateCacheEntry
	cacheTTL                        time.Duration
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
	return &HTTPClient{base: strings.TrimRight(base, "/"), begin: strings.TrimRight(public, "/") + "/oauth2/session-links/begin", clientID: clientID, redirect: redirect, http: &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, cache: make(map[[32]byte]stateCacheEntry), cacheTTL: stateCacheTTL()}, nil
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
	err := c.bounded(ctx, completeTimeout, http.MethodPost, "/oauth2/session-links/complete", "Bearer "+access, map[string]string{"ticket": ticket, "id_token": id}, &result)
	if err == nil && result.Token == "" {
		err = ErrUnavailable
	}
	return result, err
}

// State 用途：只读查询中央生命周期，不推进任何期限。
// 参数：ctx为请求上下文，binding为服务器端解密的中央绑定秘密。
// 返回值：权威窗口；失效返回ErrInactive，网络或平台故障返回ErrUnavailable。
// 只读窗口可在短TTL内复用：只有成功响应进入缓存，失效/冲突删除条目，故障不写缓存也不回退旧值。
func (c *HTTPClient) State(ctx context.Context, binding string) (Context, error) {
	var key [32]byte
	if binding != "" {
		key = sha256.Sum256([]byte(binding))
		if value, ok := c.cachedState(key); ok {
			return value, nil
		}
	}
	var result Context
	err := c.bounded(ctx, stateTimeout, http.MethodGet, "/oauth2/session-context", "SessionBinding "+binding, nil, &result)
	switch {
	case err == nil:
		if binding != "" {
			c.storeState(key, result)
		}
	case errors.Is(err, ErrInactive), errors.Is(err, ErrBindingConflict):
		if binding != "" {
			c.dropState(key)
		}
	}
	return result, err
}

// Activity 用途：代理同源已认证页面的真实交互到中央会话。
// 参数：ctx为请求上下文，binding为服务器端解密的中央绑定秘密。
// 返回值：续期后的权威窗口；已失效会话不可复活。交互必须真实到达平台，因此不读也不写缓存。
func (c *HTTPClient) Activity(ctx context.Context, binding string) (Context, error) {
	var result Context
	err := c.bounded(ctx, activityTimeout, http.MethodPost, "/oauth2/session/activity", "SessionBinding "+binding, nil, &result)
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

// bounded 用途：为单次协议调用套上按方法区分的有界超时。
// 参数：ctx为调用方上下文，timeout为该方法的期限，其余同request。
// 返回值：request的错误分类结果。
func (c *HTTPClient) bounded(ctx context.Context, timeout time.Duration, method, path, authorization string, body any, target any) error {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.request(callCtx, method, path, authorization, body, target)
}

// cachedState 用途：在TTL内返回已缓存的只读窗口，过期条目即时清理。
// 参数：key为binding摘要。
// 返回值：命中且未过期时返回窗口副本。
func (c *HTTPClient) cachedState(key [32]byte) (Context, bool) {
	if c.cacheTTL <= 0 {
		return Context{}, false
	}
	now := time.Now()
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	entry, ok := c.cache[key]
	if !ok {
		return Context{}, false
	}
	if now.Sub(entry.at) >= c.cacheTTL {
		delete(c.cache, key)
		return Context{}, false
	}
	return entry.value, true
}

// storeState 用途：只缓存成功响应；条目达到上限时整体清空，不做LRU。
// 参数：key为binding摘要，value为权威窗口。
// 返回值：无。
func (c *HTTPClient) storeState(key [32]byte, value Context) {
	if c.cacheTTL <= 0 {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if len(c.cache) >= stateCacheMaxItems {
		c.cache = make(map[[32]byte]stateCacheEntry)
	}
	c.cache[key] = stateCacheEntry{value: value, at: time.Now()}
}

// dropState 用途：确定失效或绑定冲突后删除缓存，避免旧窗口继续被复用。
// 参数：key为binding摘要。
// 返回值：无。
func (c *HTTPClient) dropState(key [32]byte) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	delete(c.cache, key)
}

// stateCacheTTL 用途：读取只读窗口缓存TTL，非法值回落默认并告警。
// 参数：无，环境变量CENTRAL_SESSION_CACHE_MS单位毫秒，0表示关闭。
// 返回值：0到5秒之间的TTL。
func stateCacheTTL() time.Duration {
	raw, ok := os.LookupEnv(stateCacheEnv)
	raw = strings.TrimSpace(raw)
	if !ok || raw == "" {
		return stateCacheDefaultMS * time.Millisecond
	}
	ms, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("[WARN] sessiongovernance: invalid %s=%q, using %dms", stateCacheEnv, raw, stateCacheDefaultMS)
		return stateCacheDefaultMS * time.Millisecond
	}
	if ms < 0 {
		log.Printf("[WARN] sessiongovernance: %s=%d below range, clamped to 0ms", stateCacheEnv, ms)
		ms = 0
	}
	if ms > stateCacheMaxMS {
		log.Printf("[WARN] sessiongovernance: %s=%d above range, clamped to %dms", stateCacheEnv, ms, stateCacheMaxMS)
		ms = stateCacheMaxMS
	}
	return time.Duration(ms) * time.Millisecond
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
