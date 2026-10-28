package platform

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/migrations"
	"golang.org/x/oauth2"
)

// authTimeDatabase 用途：仅在显式loopback测试库中新建并清理本轮独占schema。
// 参数：t 为测试上下文，zone 为连接时区。
// 返回值：返回真实MySQL连接，测试结束删除本轮schema。
func authTimeDatabase(t *testing.T, zone string) *sql.DB {
	t.Helper()
	raw := os.Getenv("SETTLEMENT_E2E_TEST_DSN")
	if raw == "" {
		t.Skip("explicit isolated MySQL DSN required")
	}
	cfg, err := driver.ParseDSN(raw)
	if err != nil {
		t.Fatal("invalid isolated DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || host != "127.0.0.1" || cfg.DBName != "settlement_e2e" {
		t.Fatal("isolated loopback settlement_e2e required")
	}
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open test control connection")
	}
	name := fmt.Sprintf("settlement_oidc_%x", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatal("create owned schema failed")
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Error("owned schema cleanup failed")
		}
		admin.Close()
	})
	cfg.DBName = name
	cfg.ParseTime = true
	cfg.Loc, err = time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open owned schema")
	}
	t.Cleanup(func() { db.Close() })
	rawSQL, err := migrations.Files.ReadFile("000002_invoice_dunning_auth.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range strings.Split(string(rawSQL), ";")[:2] {
		if _, err = db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// TestAuthenticateSessionAcrossMySQLTimezones 用途：验证非UTC连接下新会话可用、空闲超时与绝对到期仍拒绝。
// 参数：t 为隔离测试上下文。
// 返回值：无；授权上下文使用明确替身，SQL与加密会话真实执行。
func TestAuthenticateSessionAcrossMySQLTimezones(t *testing.T) {
	for _, zone := range []string{"UTC", "Asia/Shanghai", "America/New_York"} {
		t.Run(zone, func(t *testing.T) {
			db := authTimeDatabase(t, zone)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth2/authorization-context" || r.Header.Get("Authorization") != "Bearer isolated-token" {
					http.Error(w, "denied", 403)
					return
				}
				json.NewEncoder(w).Encode(authorizationContext{Subject: "op-user", SubjectID: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", ClientID: "settlement-web", ApplicationCode: "settlement", EnvironmentCode: "prod", CatalogVersion: "2", RoleConfigHash: "settlement-v2-invoice-read", CompatibleCatalogVersions: []string{"2"}, CompatibleRoleConfigHashes: []string{"settlement-v2-invoice-read"}, AuthorizationRevision: 1, Permissions: []string{"settlement.invoice.read"}})
			}))
			defer server.Close()
			block, _ := aes.NewCipher(make([]byte, 32))
			codec, _ := cipher.NewGCM(block)
			a := &Authenticator{db: db, codec: codec, client: server.Client(), cfg: config.Config{PlatformBaseURL: server.URL, OIDCClientID: "settlement-web", OIDCEnvironmentCode: "prod", OIDCSessionCookieName: "settlement_session", OIDCSessionIdleTTL: 30 * time.Minute}}
			now := time.Now().UTC()
			for _, scenario := range []string{"fresh", "idle-expired", "absolute-expired", "token-expired", "revoked"} {
				t.Run(scenario, func(t *testing.T) {
					last, expires, tokenExpiry := now, now.Add(time.Hour), now.Add(time.Hour)
					var revoked any
					switch scenario {
					case "idle-expired":
						last = now.Add(-31 * time.Minute)
					case "absolute-expired":
						expires = now.Add(-time.Second)
					case "token-expired":
						tokenExpiry = now.Add(-time.Second)
					case "revoked":
						revoked = now.Format("2006-01-02 15:04:05.000")
					}
					principalJSON, _ := json.Marshal(service.Principal{TenantID: "tenant-1", UserID: "identity-1", IdentityID: "identity-1"})
					tokenJSON, _ := json.Marshal(oauth2.Token{AccessToken: "isolated-token", Expiry: tokenExpiry})
					cookie := scenario
					format := func(at time.Time) string { return at.UTC().Format("2006-01-02 15:04:05.000") }
					_, err := db.Exec(`INSERT INTO settlement_oidc_session(session_id_hash,tenant_id,identity_id,principal_json,oauth_token_ciphertext,id_token_ciphertext,authorization_revision,authorization_checked_at,token_expires_at,session_expires_at,created_at,last_seen_at,revoked_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, digest(cookie), "tenant-1", "identity-1", principalJSON, a.encrypt(tokenJSON), []byte{}, 1, format(now), format(tokenExpiry), format(expires), format(now), format(last), revoked)
					if err != nil {
						t.Fatal(err)
					}
					request := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
					request.AddCookie(&http.Cookie{Name: "settlement_session", Value: cookie})
					principal, err := a.Authenticate(context.Background(), request)
					if scenario == "fresh" {
						if err != nil || principal.UserID != "identity-1" {
							t.Fatalf("fresh session rejected with driver timezone %s: %v", zone, err)
						}
					} else if !errors.Is(err, ErrUnauthenticated) {
						t.Fatalf("%s was not rejected: %v", scenario, err)
					}
				})
			}
		})
	}
}
