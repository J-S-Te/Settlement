package platform

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/config"
)

type logoutSQLCall struct {
	query string
	args  []driver.NamedValue
}

// 只替换数据库边界；AEAD、会话绑定与 HTTP 重定向均执行生产代码。
type logoutTestDB struct {
	identity          string
	idToken           []byte
	missing           bool
	queryErr, execErr error
	affected          int64
	queries, writes   []logoutSQLCall
}

func (d *logoutTestDB) Connect(context.Context) (driver.Conn, error) { return &logoutTestConn{d}, nil }
func (d *logoutTestDB) Driver() driver.Driver                        { return logoutTestDriver{} }

type logoutTestDriver struct{}

func (logoutTestDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type logoutTestConn struct{ db *logoutTestDB }

func (*logoutTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*logoutTestConn) Close() error              { return nil }
func (*logoutTestConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected transaction") }
func (c *logoutTestConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.db.queries = append(c.db.queries, logoutSQLCall{q, args})
	if c.db.queryErr != nil {
		return nil, c.db.queryErr
	}
	return &logoutTestRows{identity: c.db.identity, token: c.db.idToken, done: c.db.missing}, nil
}
func (c *logoutTestConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.db.writes = append(c.db.writes, logoutSQLCall{q, args})
	return driver.RowsAffected(c.db.affected), c.db.execErr
}

type logoutTestRows struct {
	identity string
	token    []byte
	done     bool
}

func (*logoutTestRows) Columns() []string { return []string{"identity_id", "id_token_ciphertext"} }
func (*logoutTestRows) Close() error      { return nil }
func (r *logoutTestRows) Next(values []driver.Value) error {
	if r.done {
		return io.EOF
	}
	values[0], values[1], r.done = r.identity, r.token, true
	return nil
}

func logoutFixture(t *testing.T) (*Authenticator, *logoutTestDB, string) {
	t.Helper()
	block, err := aes.NewCipher(bytes.Repeat([]byte{0x35}, 32))
	if err != nil {
		t.Fatal(err)
	}
	codec, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	d := &logoutTestDB{identity: "identity-1", affected: 1}
	db := sql.OpenDB(d)
	t.Cleanup(func() { _ = db.Close() })
	a := &Authenticator{db: db, codec: codec, cfg: config.Config{
		OIDCIssuer: "https://identity.example.test/realms/main", OIDCClientID: "settlement-web", OIDCTenantID: "tenant-1",
		PublicOrigin: "https://finance.example.test", OIDCPostLogoutURI: "https://finance.example.test/settlement/",
		OIDCSessionCookieName: "settlement_session", OIDCSessionSecure: true,
	}, endSession: "https://identity.example.test/realms/main/logout?provider=keycloak"}
	raw := storedLogoutToken(a.cfg, nil)
	d.idToken = a.encrypt([]byte(raw))
	return a, d, raw
}

func storedLogoutToken(cfg config.Config, override map[string]any) string {
	claims := map[string]any{"iss": cfg.OIDCIssuer, "aud": cfg.OIDCClientID, "tenant_id": cfg.OIDCTenantID,
		"identity_id": "identity-1", "sub": "provider-subject-1", "exp": time.Now().Add(time.Hour).Unix()}
	for key, value := range override {
		claims[key] = value
	}
	payload, _ := json.Marshal(claims)
	// 测试替身代表 Callback 已验签并加密的令牌；本测试不伪称执行 OP 验签。
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".test-signature"
}

func logoutRequest(a *Authenticator) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://finance.example.test/settlement/auth/logout", nil)
	r.AddCookie(&http.Cookie{Name: a.cfg.OIDCSessionCookieName, Value: "opaque-local-session"})
	return r
}

func assertLocalLogout(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/settlement/logged-out" {
		t.Fatalf("local fallback = %d %q", w.Code, w.Header().Get("Location"))
	}
	assertExpiredLogoutCookie(t, w)
}

func assertExpiredLogoutCookie(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != "" || cookies[0].MaxAge >= 0 || cookies[0].Path != "/settlement" || !cookies[0].HttpOnly {
		t.Fatalf("local session cookie was not expired safely: %#v", cookies)
	}
}

func logoutLocation(t *testing.T, w *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil || w.Code != http.StatusFound || u.Host == "" {
		t.Fatalf("expected RP redirect, got %d %q", w.Code, w.Header().Get("Location"))
	}
	return u
}

func TestLogoutUsesDiscoveredEndpointAndStoredHint(t *testing.T) {
	a, d, _ := logoutFixture(t)
	var issuer string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			t.Errorf("unexpected provider request: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
			"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "end_session_endpoint": issuer + "/logout?provider=keycloak"})
	}))
	defer provider.Close()
	issuer = provider.URL
	cfg := a.cfg
	cfg.OIDCIssuer, cfg.AllowInsecureHTTPOrigin = issuer, true
	cfg.OIDCSessionKey = bytes.Repeat([]byte{0x35}, 32)
	a, err := NewAuthenticator(context.Background(), a.db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw := storedLogoutToken(cfg, nil)
	d.idToken = a.encrypt([]byte(raw))
	w := httptest.NewRecorder()
	a.Logout(w, logoutRequest(a))
	u := logoutLocation(t, w)
	if u.Scheme+"://"+u.Host != issuer || u.Path != "/logout" || u.Query().Get("provider") != "keycloak" ||
		u.Query().Get("client_id") != "settlement-web" || u.Query().Get("id_token_hint") != raw ||
		u.Query().Get("post_logout_redirect_uri") != "https://finance.example.test/settlement/" {
		t.Fatal("RP logout did not preserve the trusted discovery/client/hint/registered redirect contract")
	}
	assertExpiredLogoutCookie(t, w)
}

func TestLogoutRejectsUntrustedEndSessionEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "javascript:alert(1)", "//identity.example.test/logout", "https:///logout", "https://evil.example.test/logout",
		"http://identity.example.test/logout", "https://user@identity.example.test/logout", "https://identity.example.test/logout#fragment",
		"https://identity.example.test/logout#", "https://identity.example.test/logout?broken=%GG", "https://identity.example.test:8443/logout"} {
		t.Run(endpoint, func(t *testing.T) {
			a, _, _ := logoutFixture(t)
			a.endSession = endpoint
			w := httptest.NewRecorder()
			a.Logout(w, logoutRequest(a))
			assertLocalLogout(t, w)
		})
	}
}

func TestLogoutHTTPRequiresExistingExplicitAllowance(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		a, d, _ := logoutFixture(t)
		a.cfg.OIDCIssuer, a.endSession = "http://identity.example.test/realm", "http://identity.example.test/realm/logout"
		a.cfg.AllowInsecureHTTPOrigin = allowed
		d.idToken = a.encrypt([]byte(storedLogoutToken(a.cfg, nil)))
		w := httptest.NewRecorder()
		a.Logout(w, logoutRequest(a))
		if !allowed {
			assertLocalLogout(t, w)
		} else if u := logoutLocation(t, w); u.Scheme != "http" || u.Host != "identity.example.test" {
			t.Fatal("explicitly permitted HTTP issuer logout was not used")
		}
	}
}

func TestLogoutStoredHintIsBoundToIssuerClientTenantAndIdentity(t *testing.T) {
	for name, claims := range map[string]map[string]any{
		"issuer": {"iss": "https://other.example.test"}, "client": {"aud": "other-client"},
		"tenant": {"tenant_id": "other-tenant"}, "identity": {"identity_id": "other-identity"}, "subject": {"sub": ""},
	} {
		t.Run(name, func(t *testing.T) {
			a, d, _ := logoutFixture(t)
			d.idToken = a.encrypt([]byte(storedLogoutToken(a.cfg, claims)))
			w := httptest.NewRecorder()
			a.Logout(w, logoutRequest(a))
			assertLocalLogout(t, w)
		})
	}
}

func TestLogoutDoesNotRequireStoredIDTokenToBeUnexpired(t *testing.T) {
	a, d, _ := logoutFixture(t)
	raw := storedLogoutToken(a.cfg, map[string]any{"exp": 1, "aud": []string{"another-audience", "settlement-web"}})
	d.idToken = a.encrypt([]byte(raw))
	w := httptest.NewRecorder()
	a.Logout(w, logoutRequest(a))
	if u := logoutLocation(t, w); u.Query().Get("id_token_hint") != raw {
		t.Fatal("expired but authenticated stored hint was discarded")
	}
}

func TestLogoutUsesCanonicalPrincipalIdentityWithDifferentProviderSubject(t *testing.T) {
	for _, subjectID := range []string{"", "identity-1"} {
		a, d, _ := logoutFixture(t)
		claims := oidcClaims{Subject: "provider-subject-1", IdentityID: "identity-1", TenantID: "tenant-1"}
		authz := authorizationContext{Subject: "provider-subject-1", SubjectID: subjectID, IdentityID: "identity-1", TenantID: "tenant-1"}
		canonical, err := canonicalSubjectID(authz.SubjectID, authz.IdentityID)
		if err != nil {
			t.Fatal(err)
		}
		p := principal(authz, claims)
		p.UserID = canonical // 与 Callback 写会话时使用的真实映射一致。
		d.identity = p.UserID
		raw := storedLogoutToken(a.cfg, map[string]any{"sub": claims.Subject, "identity_id": claims.IdentityID})
		d.idToken = a.encrypt([]byte(raw))
		w := httptest.NewRecorder()
		a.Logout(w, logoutRequest(a))
		if u := logoutLocation(t, w); u.Query().Get("id_token_hint") != raw {
			t.Fatal("canonical platform identity was confused with the distinct provider subject")
		}
	}
}

func TestLogoutNormalizesDefaultPortsWithoutChangingConfiguredRedirect(t *testing.T) {
	for _, scheme := range []string{"https", "http"} {
		a, d, _ := logoutFixture(t)
		port := "443"
		if scheme == "http" {
			port = "80"
			a.cfg.AllowInsecureHTTPOrigin = true
		}
		a.cfg.OIDCIssuer = scheme + "://identity.example.test/realm"
		a.endSession = scheme + "://IDENTITY.example.test:" + port + "/realm/logout"
		a.cfg.PublicOrigin = scheme + "://finance.example.test"
		a.cfg.OIDCPostLogoutURI = scheme + "://finance.example.test:" + port + "/settlement/logged-out"
		d.idToken = a.encrypt([]byte(storedLogoutToken(a.cfg, nil)))
		w := httptest.NewRecorder()
		a.Logout(w, logoutRequest(a))
		u := logoutLocation(t, w)
		if u.Query().Get("post_logout_redirect_uri") != a.cfg.OIDCPostLogoutURI {
			t.Fatal("equivalent default ports changed or discarded the exact configured post logout URI")
		}
	}
}

func TestLogoutNeverUsesRequestHostHintReturnToOrState(t *testing.T) {
	a, _, raw := logoutFixture(t)
	a.endSession += "&state=metadata-state&post_logout_redirect_uri=https://evil.example.test/"
	r := logoutRequest(a)
	r.Host = "evil.example.test"
	r.Header.Set("X-Forwarded-Host", "evil.example.test")
	r.URL.RawQuery = "id_token_hint=attacker-token&return_to=https://evil.example.test/&post_logout_redirect_uri=https://evil.example.test/&state=attacker-state"
	w := httptest.NewRecorder()
	a.Logout(w, r)
	u := logoutLocation(t, w)
	if u.Host != "identity.example.test" || u.Query().Get("id_token_hint") != raw ||
		u.Query().Get("post_logout_redirect_uri") != "https://finance.example.test/settlement/" || u.Query().Get("state") != "" {
		t.Fatal("untrusted request or stale discovery state affected RP logout parameters")
	}
}

func TestLogoutConfiguredPostRedirectMustBeSafeAndSameOrigin(t *testing.T) {
	for _, candidate := range []string{"", "/settlement/", "//evil.example.test/", "https://evil.example.test/settlement/",
		"https://user@finance.example.test/settlement/", "https://finance.example.test/settlement/#fragment", "https://finance.example.test/settlement/#", "javascript:alert(1)", "https://finance.example.test:8443/settlement/"} {
		t.Run(candidate, func(t *testing.T) {
			a, _, _ := logoutFixture(t)
			a.cfg.OIDCPostLogoutURI = candidate
			w := httptest.NewRecorder()
			a.Logout(w, logoutRequest(a))
			if u := logoutLocation(t, w); u.Query().Get("post_logout_redirect_uri") != "" {
				t.Fatal("unsafe or cross-origin configured post logout URI was forwarded")
			}
		})
	}
	for _, candidate := range []string{"https://finance.example.test/settlement/", "https://finance.example.test/settlement/logged-out"} {
		a, _, _ := logoutFixture(t)
		a.cfg.OIDCPostLogoutURI = candidate
		w := httptest.NewRecorder()
		a.Logout(w, logoutRequest(a))
		if u := logoutLocation(t, w); u.Query().Get("post_logout_redirect_uri") != candidate {
			t.Fatal("existing configured post logout URI was changed")
		}
	}
}

func TestLogoutMissingOrUnreadableHintFallsBackAfterLocalRevocation(t *testing.T) {
	for _, scenario := range []string{"missing", "query error", "corrupt cipher", "invalid token", "already revoked"} {
		t.Run(scenario, func(t *testing.T) {
			a, d, _ := logoutFixture(t)
			switch scenario {
			case "missing":
				d.missing = true
			case "query error":
				d.queryErr = errors.New("storage read unavailable")
			case "corrupt cipher":
				d.idToken = []byte("invalid ciphertext")
			case "invalid token":
				d.idToken = a.encrypt([]byte("invalid token"))
			case "already revoked":
				d.affected = 0
			}
			w := httptest.NewRecorder()
			a.Logout(w, logoutRequest(a))
			assertLocalLogout(t, w)
			if len(d.writes) != 1 {
				t.Fatal("missing hint must not prevent local revocation")
			}
		})
	}
}

func TestLogoutAndLocalLogoutPreserveCookieWhenRevocationFails(t *testing.T) {
	for _, local := range []bool{false, true} {
		a, d, _ := logoutFixture(t)
		d.execErr = errors.New("database unavailable")
		w := httptest.NewRecorder()
		if local {
			a.LogoutLocal(w, logoutRequest(a))
		} else {
			a.Logout(w, logoutRequest(a))
		}
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Location") != "" {
			t.Fatalf("failed local revoke falsely completed logout: status=%d cleared_cookie=%t redirect=%t", w.Code, w.Header().Get("Set-Cookie") != "", w.Header().Get("Location") != "")
		}
	}
}

func TestLogoutLocalKeepsExistingLocalOnlyContract(t *testing.T) {
	a, d, _ := logoutFixture(t)
	w := httptest.NewRecorder()
	a.LogoutLocal(w, logoutRequest(a))
	if w.Code != http.StatusNoContent || w.Header().Get("Location") != "" || len(d.queries) != 0 || len(d.writes) != 1 {
		t.Fatal("local logout changed into an RP logout or read an unnecessary ID token")
	}
	assertExpiredLogoutCookie(t, w)
}

func TestLogoutDatabaseReadsAndRevocationStayTenantBound(t *testing.T) {
	for _, local := range []bool{false, true} {
		a, d, _ := logoutFixture(t)
		w := httptest.NewRecorder()
		if local {
			a.LogoutLocal(w, logoutRequest(a))
		} else {
			a.Logout(w, logoutRequest(a))
		}
		if len(d.writes) != 1 || !local && len(d.queries) != 1 {
			t.Fatal("logout did not execute its required local session operations")
		}
		for _, call := range append(d.queries, d.writes...) {
			if !strings.Contains(call.query, "tenant_id=?") || len(call.args) != 2 || call.args[1].Value != "tenant-1" {
				t.Fatal("logout SQL did not bind the configured tenant")
			}
			hash, ok := call.args[0].Value.([]byte)
			if !ok || !bytes.Equal(hash, digest("opaque-local-session")) {
				t.Fatal("logout SQL did not bind the opaque cookie hash")
			}
		}
	}
}

func TestLogoutWithoutLocalCookieIsIdempotentAndDoesNotContactOP(t *testing.T) {
	for _, local := range []bool{false, true} {
		a, d, _ := logoutFixture(t)
		r := httptest.NewRequest(http.MethodGet, "/auth/logout?id_token_hint=attacker-token", nil)
		w := httptest.NewRecorder()
		if local {
			a.LogoutLocal(w, r)
			if w.Code != http.StatusNoContent || w.Header().Get("Location") != "" {
				t.Fatal("missing-cookie local logout was not idempotent")
			}
		} else {
			a.Logout(w, r)
			assertLocalLogout(t, w)
		}
		if len(d.queries) != 0 || len(d.writes) != 0 {
			t.Fatal("missing-cookie logout touched a server session")
		}
	}
}
