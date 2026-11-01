package platform

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	provider "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// stateCookie 将原始state绑定到发起登录的浏览器，阻止跨浏览器票据注入。
// 参数：state为原始随机值，maxAge为有效秒数或删除标记。
// 返回值：与结算Cookie路径及安全属性一致的HttpOnly状态Cookie。
func (a *Authenticator) stateCookie(state string, maxAge int) *http.Cookie {
	cookie := a.cookie(state, time.Now().UTC().Add(time.Duration(maxAge)*time.Second))
	cookie.Name = a.cfg.OIDCSessionCookieName + "_oidc_state"
	cookie.MaxAge = maxAge
	return cookie
}

// attachSessionLink 原子保存票据密文，再用原nonce及PKCE继续提供方认证。
// 参数：w/r为浏览器请求响应，state为Cookie已核验的原值，ticket为平台随机票据。
// 返回值：无；重放、过期或数据库失败时不继续授权。
func (a *Authenticator) attachSessionLink(w http.ResponseWriter, r *http.Request, state, ticket string) {
	if r.URL.Query().Get("code") != "" || len(ticket) > 128 {
		http.Error(w, "invalid session ticket", 401)
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "session storage unavailable", 503)
		return
	}
	defer tx.Rollback()
	var nonceCipher, verifierCipher, ticketCipher []byte
	err = tx.QueryRowContext(r.Context(), `SELECT nonce_ciphertext,code_verifier_ciphertext,central_ticket_ciphertext
	 FROM settlement_oidc_login_transaction WHERE state_hash=? AND tenant_id=? AND consumed_at IS NULL AND expires_at>UTC_TIMESTAMP(3) FOR UPDATE`, digest(state), a.cfg.OIDCTenantID).Scan(&nonceCipher, &verifierCipher, &ticketCipher)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "session storage unavailable", http.StatusServiceUnavailable)
		return
	}
	if errors.Is(err, sql.ErrNoRows) || len(ticketCipher) > 0 {
		http.Error(w, "invalid session ticket", 401)
		return
	}
	if _, err = tx.ExecContext(r.Context(), "UPDATE settlement_oidc_login_transaction SET central_ticket_ciphertext=? WHERE state_hash=?", a.encrypt([]byte(ticket)), digest(state)); err != nil {
		http.Error(w, "session storage unavailable", 503)
		return
	}
	nonce, err := a.decrypt(nonceCipher)
	if err != nil {
		http.Error(w, "invalid login state", 401)
		return
	}
	verifier, err := a.decrypt(verifierCipher)
	if err != nil {
		http.Error(w, "invalid login state", 401)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "session storage unavailable", 503)
		return
	}
	target := a.oauth.AuthCodeURL(state, provider.Nonce(string(nonce)), oauth2.S256ChallengeOption(string(verifier)), oauth2.SetAuthURLParam("claims", `{"id_token":{"auth_time":{"essential":true}}}`))
	http.Redirect(w, r, target, http.StatusFound)
}

// SessionState 返回本Cookie精确关联的中央窗口，不更新真实活动。
// 参数：w/r为HTTP请求响应，r携带结算HttpOnly Cookie。
// 返回值：无；确定失效401，依赖故障503。
func (a *Authenticator) SessionState(w http.ResponseWriter, r *http.Request) {
	a.lifecycle(w, r, false)
}

// Activity 接收受Origin与CSRF检查的真实交互，不复活已失效周期。
// 参数：w/r为HTTP请求响应，r须携带X-CSRF-Token:1。
// 返回值：无；成功输出不含秘密的中央生命周期。
func (a *Authenticator) Activity(w http.ResponseWriter, r *http.Request) { a.lifecycle(w, r, true) }

// lifecycle 保留结算的独立Cookie及授权边界，只代理中央状态或活动。
// 参数：w/r为HTTP边界，interact区分只读与交互。
// 返回值：无；失败保持准确状态且不泄漏提供方材料。
func (a *Authenticator) lifecycle(w http.ResponseWriter, r *http.Request, interact bool) {
	w.Header().Set("Cache-Control", "no-store")
	if a.central == nil {
		http.Error(w, "central sessions not enabled", 503)
		return
	}
	if interact {
		origin, err := url.Parse(r.Header.Get("Origin"))
		expected, expectedErr := url.Parse(a.cfg.PublicOrigin)
		if err != nil || expectedErr != nil || origin.Scheme != expected.Scheme || origin.Host != expected.Host || r.Header.Get("X-CSRF-Token") != "1" {
			http.Error(w, "forbidden", 403)
			return
		}
	}
	if _, err := a.Authenticate(r.Context(), r); err != nil {
		status := 503
		if errors.Is(err, ErrUnauthenticated) {
			status = 401
		}
		http.Error(w, "session unavailable", status)
		return
	}
	cookie, err := r.Cookie(a.cfg.OIDCSessionCookieName)
	if err != nil {
		http.Error(w, "unauthenticated", 401)
		return
	}
	row, err := loadCentralSession(r.Context(), a.db, digest(cookie.Value), false)
	if err != nil {
		http.Error(w, "session unavailable", 503)
		return
	}
	state, err := a.readCentral(r.Context(), row, interact)
	if err != nil {
		status := 503
		if errors.Is(err, ErrUnauthenticated) {
			status = 401
		}
		http.Error(w, "session unavailable", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": "OK", "message": "操作成功", "data": state})
}
