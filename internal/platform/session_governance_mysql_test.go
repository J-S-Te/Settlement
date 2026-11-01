package platform

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	central "github.com/j-s-te/settlement/internal/sessiongovernance"
	"golang.org/x/oauth2"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/migration"
	"github.com/j-s-te/settlement/migrations"
)

// centralMySQLDatabase 只在显式独立测试库应用真实迁移，不寻找业务配置。
// 参数：t为测试上下文；DSN仅来自SESSION_GOVERNANCE_TEST_DSN。
// 返回值：真实OIDC存储；无专用DSN则跳过，不符合回环隔离边界则失败。
func centralMySQLDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SESSION_GOVERNANCE_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated SESSION_GOVERNANCE_TEST_DSN is not configured")
	}
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid isolated DSN")
	}
	host, portText, err := net.SplitHostPort(config.Addr)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || config.Net != "tcp" || host != "127.0.0.1" ||
		port < 32768 || config.DBName != "qa_session_settlement_20261101" {
		t.Fatal("refusing non-isolated session DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal("migration rerun:", err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(16)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// refreshFixtureID 签发独立测试IdP的真实RSA身份令牌供刷新路径完整验签。
// 参数：t为测试上下文，now为本轮固定认证时间。
// 返回值：签名身份令牌及只信任该临时公钥的验证器。
func refreshFixtureID(t *testing.T, now time.Time) (string, *oidc.IDTokenVerifier) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"iss": "https://isolated-issuer.test", "aud": "qa-client",
		"sub": "qa-identity", "identity_id": "qa-identity", "tenant_id": "qa-session", "sid": "qa-sid",
		"token_use": "id_token", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	message, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := message.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	verifier := oidc.NewVerifier("https://isolated-issuer.test", &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}},
		&oidc.Config{ClientID: "qa-client", SupportedSigningAlgs: []string{"RS256"}, Now: func() time.Time { return now }})
	return raw, verifier
}

type settlementFixedCentral struct {
	central.Client
	state central.Context
}

// State 提供本轮原始周期供结算认证链核验。
// 参数：请求上下文及服务器绑定秘密。
// 返回值：固定原始截止的非敏感窗口。
func (c settlementFixedCentral) State(context.Context, string) (central.Context, error) {
	return c.state, nil
}

// TestCentralSessionRotationCheckpointMySQL 验证结算在单次refresh后权限503仍保留新令牌。
// 参数：t为隔离MySQL上下文；IdP为临时HTTP/RSA替身，真实Keycloak另验。
// 返回值：无；重复轮换、错误退出、权限伪更新或原始截止变化时失败。
func TestCentralSessionRotationCheckpointMySQL(t *testing.T) {
	db := centralMySQLDatabase(t)
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	rawID, verifier := refreshFixtureID(t, now)
	var refreshes atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/token" {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
			return
		}
		if refreshes.Add(1) != 1 || r.FormValue("refresh_token") != "isolated-old-refresh" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "isolated-access", "refresh_token": "isolated-new-refresh", "token_type": "Bearer", "expires_in": 300, "id_token": rawID})
	}))
	t.Cleanup(issuer.Close)
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	codec, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	auth := &Authenticator{db: db, codec: codec, verifier: verifier, client: issuer.Client(),
		cfg:   config.Config{OIDCTenantID: "qa-session", OIDCClientID: "qa-client", OIDCEnvironmentCode: "test", OIDCSessionCookieName: "qa-session", PlatformBaseURL: issuer.URL},
		oauth: oauth2.Config{ClientID: "qa-client", Endpoint: oauth2.Endpoint{TokenURL: issuer.URL + "/token", AuthStyle: oauth2.AuthStyleInHeader}},
		central: settlementFixedCentral{state: central.Context{SessionID: "qa-central", TenantID: "qa-session", SubjectID: "qa-identity", ClientID: "qa-client",
			ApplicationCode: "settlement", EnvironmentCode: "test", PolicyVersion: 1, IdleTimeoutSeconds: 1800, IdleRemainingMS: 1800000,
			AbsoluteRemainingMS: 28800000, AbsoluteExpiresAt: now.Add(8 * time.Hour)}}}
	cookie := "isolated-settlement-rotation-" + now.Format("150405.000")
	hash := digest(cookie)
	tokenCipher := auth.encrypt(mustJSON(&oauth2.Token{AccessToken: "isolated-old-access", RefreshToken: "isolated-old-refresh", Expiry: now.Add(30 * time.Second)}))
	_, err = db.ExecContext(ctx, `INSERT INTO settlement_oidc_session
	(session_id_hash,tenant_id,identity_id,principal_json,oauth_token_ciphertext,id_token_ciphertext,
	 authorization_revision,authorization_checked_at,token_expires_at,session_expires_at,created_at,last_seen_at,
	 central_binding_ciphertext,central_session_id,oidc_subject,oidc_sid) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		hash, "qa-session", "qa-identity", []byte(`{"identity_id":"qa-identity","tenant_id":"qa-session"}`), tokenCipher, auth.encrypt([]byte("isolated-id")),
		1, now, now.Add(30*time.Second), now.Add(8*time.Hour), now, now, auth.encrypt([]byte("isolated-server-binding")), "qa-central", "qa-identity", "qa-sid")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM settlement_oidc_session WHERE session_id_hash=?", hash) })
	var group sync.WaitGroup
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			request := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
			request.AddCookie(&http.Cookie{Name: "qa-session", Value: cookie})
			if _, err := auth.authenticateCentral(ctx, request); !errors.Is(err, ErrSessionUnavailable) {
				t.Errorf("outage must remain unavailable: %v", err)
			}
		}()
	}
	group.Wait()
	var storedCipher []byte
	var checked, expires time.Time
	var revoked sql.NullTime
	var revision uint64
	if err := db.QueryRowContext(ctx, `SELECT oauth_token_ciphertext,authorization_checked_at,authorization_revision,session_expires_at,revoked_at
	 FROM settlement_oidc_session WHERE session_id_hash=?`, hash).Scan(&storedCipher, &checked, &revision, &expires, &revoked); err != nil {
		t.Fatal(err)
	}
	plain, err := auth.decrypt(storedCipher)
	if err != nil {
		t.Fatal(err)
	}
	var rotated oauth2.Token
	if err := json.Unmarshal(plain, &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.RefreshToken != "isolated-new-refresh" || refreshes.Load() != 1 || revoked.Valid {
		t.Fatal("verified rotation was lost, replayed or revoked")
	}
	if revision != 1 || !checked.Before(now.Add(-time.Minute)) || !expires.Equal(now.Add(8*time.Hour)) {
		t.Fatal("outage bypassed revalidation or reset original expiry")
	}
}
