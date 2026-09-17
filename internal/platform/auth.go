package platform

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/service"
	"golang.org/x/oauth2"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type Authenticator struct {
	db         *sql.DB
	cfg        config.Config
	provider   *oidc.Provider
	verifier   *oidc.IDTokenVerifier
	oauth      oauth2.Config
	client     *http.Client
	codec      cipher.AEAD
	endSession string
}

func NewAuthenticator(ctx context.Context, db *sql.DB, cfg config.Config) (*Authenticator, error) {
	block, err := aes.NewCipher(cfg.OIDCSessionKey)
	if err != nil {
		return nil, err
	}
	codec, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	issuer := strings.TrimRight(cfg.OIDCIssuer, "/")
	if cfg.OIDCBackchannelBaseURL != "" {
		publicURL, err := url.Parse(issuer)
		if err != nil {
			return nil, err
		}
		backURL, err := url.Parse(strings.TrimRight(cfg.OIDCBackchannelBaseURL, "/"))
		if err != nil {
			return nil, err
		}
		client.Transport = &rewriteTransport{base: http.DefaultTransport, public: publicURL, back: backURL}
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, fmt.Errorf("load OIDC discovery: %w", err)
	}
	var discovery struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = provider.Claims(&discovery)
	return &Authenticator{db: db, cfg: cfg, provider: provider, verifier: provider.Verifier(&oidc.Config{ClientID: cfg.OIDCClientID}), client: client, codec: codec, endSession: discovery.EndSession, oauth: oauth2.Config{ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret, RedirectURL: cfg.OIDCRedirectURI, Endpoint: provider.Endpoint(), Scopes: []string{"openid", "profile"}}}, nil
}
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	state, _ := random(32)
	nonce, _ := random(32)
	verifier := oauth2.GenerateVerifier()
	now := time.Now().UTC()
	_, err := a.db.ExecContext(r.Context(), `INSERT INTO settlement_oidc_login_transaction(state_hash,tenant_id,nonce_ciphertext,code_verifier_ciphertext,return_path,expires_at,created_at) VALUES(?,?,?,?,?,?,?)`, digest(state), a.cfg.OIDCTenantID, a.encrypt([]byte(nonce)), a.encrypt([]byte(verifier)), safeReturn(r.URL.Query().Get("return_to")), now.Add(10*time.Minute), now)
	if err != nil {
		http.Error(w, "login service unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}
func (a *Authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	state, code := strings.TrimSpace(r.URL.Query().Get("state")), strings.TrimSpace(r.URL.Query().Get("code"))
	if state == "" || code == "" {
		http.Error(w, "invalid callback", http.StatusBadRequest)
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "session unavailable", 503)
		return
	}
	defer tx.Rollback()
	var nonceCipher, verifierCipher []byte
	var returnPath string
	err = tx.QueryRowContext(r.Context(), `SELECT nonce_ciphertext,code_verifier_ciphertext,return_path FROM settlement_oidc_login_transaction WHERE state_hash=? AND tenant_id=? AND consumed_at IS NULL AND expires_at>UTC_TIMESTAMP(3) FOR UPDATE`, digest(state), a.cfg.OIDCTenantID).Scan(&nonceCipher, &verifierCipher, &returnPath)
	if err != nil {
		http.Error(w, "invalid or expired state", http.StatusUnauthorized)
		return
	}
	res, err := tx.ExecContext(r.Context(), `UPDATE settlement_oidc_login_transaction SET consumed_at=UTC_TIMESTAMP(3) WHERE state_hash=? AND consumed_at IS NULL`, digest(state))
	if err != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		http.Error(w, "state already consumed", http.StatusUnauthorized)
		return
	}
	nonce, err := a.decrypt(nonceCipher)
	if err != nil {
		http.Error(w, "invalid state", 401)
		return
	}
	verifier, err := a.decrypt(verifierCipher)
	if err != nil {
		http.Error(w, "invalid state", 401)
		return
	}
	ctx := oidc.ClientContext(r.Context(), a.client)
	token, err := a.oauth.Exchange(ctx, code, oauth2.VerifierOption(string(verifier)))
	if err != nil {
		http.Error(w, "token exchange failed", 401)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "id token missing", 401)
		return
	}
	idToken, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		http.Error(w, "id token invalid", 401)
		return
	}
	var claims oidcClaims
	if err = idToken.Claims(&claims); err != nil || (claims.TokenUse != "" && claims.TokenUse != "id_token") || claims.Nonce != string(nonce) || claims.TenantID != a.cfg.OIDCTenantID || claims.IdentityID == "" || claims.Subject == "" {
		http.Error(w, "identity claims invalid", 401)
		return
	}
	authz, err := a.resolveAuthorization(ctx, token.AccessToken)
	platformSubjectID, subjectErr := canonicalSubjectID(authz.SubjectID, authz.IdentityID)
	compatibilityErr := validateSettlementCatalogCompatibility(authz)
	if err != nil || subjectErr != nil || compatibilityErr != nil || authz.Subject != claims.Subject || authz.IdentityID != claims.IdentityID || authz.TenantID != claims.TenantID || authz.ClientID != a.cfg.OIDCClientID || authz.ApplicationCode != "settlement" || authz.EnvironmentCode != a.cfg.OIDCEnvironmentCode || authz.AuthorizationRevision == 0 {
		http.Error(w, "application authorization denied", 403)
		return
	}
	p := principal(authz, claims)
	p.UserID = platformSubjectID
	principalJSON, _ := json.Marshal(p)
	tokenJSON, _ := json.Marshal(token)
	rawSession, _ := random(48)
	expires := time.Now().UTC().Add(a.cfg.OIDCSessionAbsoluteTTL)
	if token.Expiry.Before(expires) {
		expires = token.Expiry
	}
	_, err = tx.ExecContext(r.Context(), `INSERT INTO settlement_oidc_session(session_id_hash,tenant_id,identity_id,principal_json,oauth_token_ciphertext,id_token_ciphertext,authorization_revision,authorization_checked_at,token_expires_at,session_expires_at,created_at,last_seen_at) VALUES(?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, digest(rawSession), p.TenantID, p.UserID, principalJSON, a.encrypt(tokenJSON), a.encrypt([]byte(raw)), authz.AuthorizationRevision, time.Now().UTC(), token.Expiry, expires)
	if err != nil {
		http.Error(w, "create session failed", 503)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "create session failed", 503)
		return
	}
	http.SetCookie(w, a.cookie(rawSession, expires))
	http.Redirect(w, r, "/settlement/"+strings.TrimLeft(returnPath, "/"), http.StatusFound)
}
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (service.Principal, error) {
	cookie, err := r.Cookie(a.cfg.OIDCSessionCookieName)
	if err != nil || cookie.Value == "" {
		return service.Principal{}, ErrUnauthenticated
	}
	var tenant, identity string
	var principalJSON, tokenCipher []byte
	var expires time.Time
	idleCutoff := time.Now().UTC().Add(-a.cfg.OIDCSessionIdleTTL)
	err = a.db.QueryRowContext(ctx, `SELECT tenant_id,identity_id,principal_json,oauth_token_ciphertext,session_expires_at FROM settlement_oidc_session WHERE session_id_hash=? AND revoked_at IS NULL AND session_expires_at>UTC_TIMESTAMP(3) AND last_seen_at>?`, digest(cookie.Value), idleCutoff).Scan(&tenant, &identity, &principalJSON, &tokenCipher, &expires)
	if err != nil {
		return service.Principal{}, ErrUnauthenticated
	}
	tokenJSON, err := a.decrypt(tokenCipher)
	if err != nil {
		return service.Principal{}, ErrUnauthenticated
	}
	var token oauth2.Token
	if json.Unmarshal(tokenJSON, &token) != nil || !token.Expiry.After(time.Now()) {
		return service.Principal{}, ErrUnauthenticated
	}
	authz, err := a.resolveAuthorization(ctx, token.AccessToken)
	platformSubjectID, subjectErr := canonicalSubjectID(authz.SubjectID, authz.IdentityID)
	compatibilityErr := validateSettlementCatalogCompatibility(authz)
	if err != nil || subjectErr != nil || compatibilityErr != nil || authz.TenantID != tenant || platformSubjectID != identity || authz.ClientID != a.cfg.OIDCClientID || authz.ApplicationCode != "settlement" || authz.EnvironmentCode != a.cfg.OIDCEnvironmentCode {
		return service.Principal{}, ErrUnauthenticated
	}
	var stored service.Principal
	if json.Unmarshal(principalJSON, &stored) != nil {
		return service.Principal{}, ErrUnauthenticated
	}
	current := principal(authz, oidcClaims{IdentityID: identity, PersonID: stored.PersonID, Name: stored.DisplayName, PreferredUsername: stored.Username})
	_, _ = a.db.ExecContext(ctx, `UPDATE settlement_oidc_session SET principal_json=?,authorization_revision=?,authorization_checked_at=UTC_TIMESTAMP(3),last_seen_at=UTC_TIMESTAMP(3) WHERE session_id_hash=?`, mustJSON(current), authz.AuthorizationRevision, digest(cookie.Value))
	return current, nil
}
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(a.cfg.OIDCSessionCookieName); err == nil {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE settlement_oidc_session SET revoked_at=UTC_TIMESTAMP(3) WHERE session_id_hash=? AND revoked_at IS NULL`, digest(cookie.Value))
	}
	expired := a.cookie("", time.Unix(1, 0))
	expired.MaxAge = -1
	http.SetCookie(w, expired)
	http.Redirect(w, r, "/settlement/logged-out", http.StatusFound)
}
func (a *Authenticator) LogoutLocal(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(a.cfg.OIDCSessionCookieName); err == nil {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE settlement_oidc_session SET revoked_at=UTC_TIMESTAMP(3) WHERE session_id_hash=? AND revoked_at IS NULL`, digest(cookie.Value))
	}
	expired := a.cookie("", time.Unix(1, 0))
	expired.MaxAge = -1
	http.SetCookie(w, expired)
	w.WriteHeader(http.StatusNoContent)
}
func (a *Authenticator) cookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{Name: a.cfg.OIDCSessionCookieName, Value: value, Path: "/settlement", Expires: expires, HttpOnly: true, Secure: a.cfg.OIDCSessionSecure, SameSite: http.SameSiteLaxMode}
}
func (a *Authenticator) encrypt(value []byte) []byte {
	nonce := make([]byte, a.codec.NonceSize())
	_, _ = rand.Read(nonce)
	return a.codec.Seal(nonce, nonce, value, nil)
}
func (a *Authenticator) decrypt(value []byte) ([]byte, error) {
	if len(value) < a.codec.NonceSize() {
		return nil, errors.New("ciphertext invalid")
	}
	return a.codec.Open(nil, value[:a.codec.NonceSize()], value[a.codec.NonceSize():], nil)
}
func digest(v string) []byte { s := sha256.Sum256([]byte(v)); return s[:] }
func random(size int) (string, error) {
	b := make([]byte, size)
	_, err := rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b), err
}
func safeReturn(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return "/"
	}
	parsed, err := url.Parse(v)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || strings.Contains(parsed.Path, "\\") {
		return "/"
	}
	cleaned := path.Clean(parsed.Path)
	if cleaned != parsed.Path || strings.Contains(parsed.Path, "..") {
		return "/"
	}
	return parsed.RequestURI()
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type rewriteTransport struct {
	base         http.RoundTripper
	public, back *url.URL
}

func (t *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if clone.URL.Scheme == t.public.Scheme && clone.URL.Host == t.public.Host {
		u := *clone.URL
		u.Scheme = t.back.Scheme
		u.Host = t.back.Host
		clone.URL = &u
		clone.Host = t.public.Host
	}
	return t.base.RoundTrip(clone)
}
