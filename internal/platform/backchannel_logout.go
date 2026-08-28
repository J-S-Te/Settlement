package platform

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const backchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"

type backchannelLogoutClaims struct {
	Subject  string                     `json:"sub"`
	JTI      string                     `json:"jti"`
	Issued   int64                      `json:"iat"`
	Expires  int64                      `json:"exp"`
	Nonce    string                     `json:"nonce"`
	Audience interface{}                `json:"aud"`
	Events   map[string]json.RawMessage `json:"events"`
}

// BackchannelLogout 接收并验证标准 OIDC logout_token，在同一事务中完成重放保护和会话撤销。
func (a *Authenticator) BackchannelLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	raw := strings.TrimSpace(r.Form.Get("logout_token"))
	if raw == "" || len(raw) > 64*1024 || !validLogoutTokenType(raw) {
		http.Error(w, "invalid logout_token", http.StatusBadRequest)
		return
	}
	token, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		http.Error(w, "invalid logout_token", http.StatusUnauthorized)
		return
	}
	var claims backchannelLogoutClaims
	if err := token.Claims(&claims); err != nil || !validLogoutClaims(claims, a.cfg.OIDCClientID, time.Now().UTC()) {
		http.Error(w, "invalid logout_token claims", http.StatusUnauthorized)
		return
	}
	claimed, err := a.processBackchannelLogout(r.Context(), claims, time.Now().UTC())
	if err != nil {
		http.Error(w, "logout storage unavailable", http.StatusServiceUnavailable)
		return
	}
	_ = claimed // 重复 JTI 同样返回 200，满足幂等接收语义。
	w.WriteHeader(http.StatusOK)
}

func (a *Authenticator) processBackchannelLogout(ctx context.Context, claims backchannelLogoutClaims, now time.Time) (bool, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_oidc_backchannel_logout_replay (jti_hash,expires_at,created_at) VALUES (?,?,?)`, hashJTI(claims.JTI), now.Add(5*time.Minute), now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			return false, nil
		}
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_oidc_session SET revoked_at=? WHERE tenant_id=? AND revoked_at IS NULL AND JSON_UNQUOTE(JSON_EXTRACT(principal_json,'$.user_id'))=?`, now, a.cfg.OIDCTenantID, claims.Subject); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func validLogoutClaims(c backchannelLogoutClaims, clientID string, now time.Time) bool {
	if c.Subject == "" || c.JTI == "" || c.Nonce != "" || c.Issued <= 0 || c.Expires <= now.Unix() || c.Expires-c.Issued > 300 {
		return false
	}
	event, ok := c.Events[backchannelLogoutEvent]
	if !ok {
		return false
	}
	var props map[string]json.RawMessage
	if json.Unmarshal(event, &props) != nil || len(props) != 0 {
		return false
	}
	switch aud := c.Audience.(type) {
	case string:
		return aud == clientID
	case []interface{}:
		for _, v := range aud {
			if s, ok := v.(string); ok && s == clientID {
				return true
			}
		}
	}
	return false
}

func validLogoutTokenType(raw string) bool {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return false
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var value struct {
		Typ string `json:"typ"`
	}
	return json.Unmarshal(header, &value) == nil && value.Typ == "logout+jwt"
}

func hashJTI(jti string) []byte { sum := sha256.Sum256([]byte(jti)); return sum[:] }
