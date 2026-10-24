package httpapi

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/migration"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/migrations"
	"golang.org/x/oauth2"
)

// 使用新建回环测试数据库及真实源 HTTP、工作流、SQL Outbox
// 产物。浏览器 OIDC 会话和授权服务器为夹具；
// 接收 API、生产机器校验器及财务事务均为真实实现。
func TestApprovedContractFinancialEventIngestConfirmSQL(t *testing.T) {
	dsn := os.Getenv("SETTLEMENT_TEST_DSN")
	artifact := os.Getenv("CONTRACT_APPROVED_FINANCIAL_EVENT_INPUT")
	if dsn == "" || artifact == "" {
		t.Skip("isolated SETTLEMENT_TEST_DSN and approved source artifact required")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("test DSN cannot be parsed")
	}
	host, _, err := net.SplitHostPort(parsed.Addr)
	if err != nil || parsed.Net != "tcp" || (host != "127.0.0.1" && host != "localhost" && host != "::1") || parsed.DBName != "settlement_p1_test_20261013" {
		t.Fatal("only exclusively allocated loopback test schema is allowed")
	}
	artifactPath, err := filepath.Abs(artifact)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := filepath.Abs("../../../.ai/qa/p1-business-fixes-20261013/agents")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(artifactPath) != owned || !strings.HasPrefix(filepath.Base(artifactPath), "contract-approved-financial-event-") {
		t.Fatal("only this QA run's approved contract artifact is allowed")
	}
	payload, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var event service.ContractEvent
	if err = json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.TenantID == "" || event.EventID == "" || len(event.Contract.PaymentTerms) != 2 || event.Contract.Amount != "100.00" {
		t.Fatal("approved native source artifact does not match the independently expected two-installment contract")
	}
	if err = migration.Run(context.Background(), dsn, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test DB: %v", err)
		}
	})
	for _, table := range []string{"settlement_contract_snapshot", "settlement_oidc_session", "settlement_contract_stream", "settlement_inbox_event"} {
		var count int
		if err = db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE tenant_id=?", event.TenantID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("owned source namespace must start empty: table=%s error=%v", table, err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"settlement_outbox_event", "settlement_receivable", "settlement_receivable_plan", "settlement_contract_snapshot", "settlement_inbox_event", "settlement_contract_stream", "settlement_oidc_session"} {
			if _, err := db.Exec("DELETE FROM "+table+" WHERE tenant_id IN (?,?)", event.TenantID, event.TenantID+"-foreign"); err != nil {
				t.Errorf("owned SQL cleanup %s: %v", table, err)
			}
		}
	})
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "platform-public.pem")
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	c := config.Config{IntegrationEnabled: true, PlatformMachineTokenPublicKeyPath: keyPath, PlatformMachineTokenIssuer: "basic-platform", ContractClientID: "contract_management-prod-settlement-integration", ContractOAuthClientID: "qa-source-oauth", ContractAudience: "basic-platform-application", OIDCTenantID: event.TenantID, ContractApplicationID: "qa-source-contract-app", ContractEnvironmentID: "qa-source-contract-prod", OIDCEnvironmentCode: "prod", OIDCClientID: "settlement-prod-web", OIDCSessionCookieName: "settlement_session", OIDCSessionKey: bytes.Repeat([]byte{17}, 32), OIDCSessionIdleTTL: time.Hour, OIDCSessionAbsoluteTTL: 2 * time.Hour, PublicOrigin: "https://qa.example.invalid", InvoiceIssuanceMode: "manual"}
	var fixture *httptest.Server
	fixture = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/.well-known/openid-configuration" {
			json.NewEncoder(w).Encode(map[string]any{"issuer": fixture.URL, "authorization_endpoint": fixture.URL + "/auth", "token_endpoint": fixture.URL + "/token", "jwks_uri": fixture.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
			return
		}
		if r.URL.Path != "/oauth2/authorization-context" {
			http.NotFound(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tenant := event.TenantID
		permission := []string{"settlement.receivable.read", "settlement.receivable.confirm"}
		if token == "no-permission" {
			permission = []string{"settlement.receivable.read"}
		}
		if token == "foreign" {
			tenant += "-foreign"
		}
		json.NewEncoder(w).Encode(map[string]any{"sub": "qa-oidc-sub", "subject_id": "qa-finance", "identity_id": "qa-finance", "tenant_id": tenant, "client_id": c.OIDCClientID, "application_code": "settlement", "environment_code": "prod", "permissions": permission, "catalog_version": "2", "compatible_catalog_versions": []string{"2"}, "role_config_hash": "settlement-v2-invoice-read", "compatible_role_config_hashes": []string{"settlement-v2-invoice-read"}, "authorization_revision": 1})
	}))
	defer fixture.Close()
	c.PlatformBaseURL = fixture.URL
	c.OIDCIssuer = fixture.URL
	auth, err := platform.NewAuthenticator(context.Background(), db, c)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := platform.NewContractServiceTokenVerifier(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(db, c, slog.New(slog.NewTextHandler(os.Stderr, nil)), auth, machine, nil)
	now := time.Now().Unix()
	claims := map[string]any{"iss": c.PlatformMachineTokenIssuer, "aud": c.ContractAudience, "token_use": "application", "sub": c.ContractClientID, "oauth_client_id": c.ContractOAuthClientID, "tenant_id": event.TenantID, "application_id": c.ContractApplicationID, "application_code": "contract_management", "environment_id": c.ContractEnvironmentID, "environment_code": "prod", "scope": []string{"settlement.contract.ingest"}, "iat": now - 1, "nbf": now - 1, "exp": now + 600}
	sign := func(claims map[string]any) string {
		body, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		input := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(body)
		return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
	}
	request := func(method, path string, body []byte, bearer, cookie string, expected int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: c.OIDCSessionCookieName, Value: cookie})
			r.Header.Set("Origin", c.PublicOrigin)
			r.Header.Set("X-CSRF-Token", "1")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != expected {
			t.Fatalf("%s %s status=%d expected=%d body=%s", method, path, w.Code, expected, w.Body.String())
		}
		return w
	}
	assertCount := func(table string, expected int) {
		t.Helper()
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE tenant_id=?", event.TenantID).Scan(&count); err != nil || count != expected {
			t.Fatalf("table=%s count=%d want=%d error=%v", table, count, expected, err)
		}
	}
	const ingest = "/internal/v1/settlement/events/contracts"
	for _, mutation := range []struct {
		field string
		value any
	}{{"sub", "foreign-client"}, {"oauth_client_id", "foreign-uid"}, {"tenant_id", "foreign-tenant"}, {"scope", []string{"settlement.contract.ingest", "settlement.contract.ingest"}}, {"exp", now}, {"token_use", "browser"}} {
		old := claims[mutation.field]
		claims[mutation.field] = mutation.value
		request("POST", ingest, payload, sign(claims), "", 401)
		claims[mutation.field] = old
	}
	assertCount("settlement_inbox_event", 0)
	assertCount("settlement_receivable_plan", 0)
	trustedToken := sign(claims)
	foreignEvent := event
	foreignEvent.TenantID += "-foreign"
	body, _ := json.Marshal(foreignEvent)
	request("POST", ingest, body, trustedToken, "", 400)
	assertCount("settlement_inbox_event", 0)
	invalidEvent := event
	invalidEvent.Contract.PaymentTerms = append([]service.PaymentTerm(nil), event.Contract.PaymentTerms...)
	invalidEvent.Contract.PaymentTerms[0].Amount = "39.00"
	body, _ = json.Marshal(invalidEvent)
	request("POST", ingest, body, trustedToken, "", 400)
	assertCount("settlement_contract_snapshot", 0)
	assertCount("settlement_contract_stream", 0)
	response := request("POST", ingest, payload, trustedToken, "", 202)
	if !strings.Contains(response.Body.String(), `"duplicate":false`) {
		t.Fatal("first event did not create state")
	}
	response = request("POST", ingest, payload, trustedToken, "", 202)
	if !strings.Contains(response.Body.String(), `"duplicate":true`) {
		t.Fatal("replay not recognized")
	}
	assertCount("settlement_inbox_event", 1)
	assertCount("settlement_contract_snapshot", 1)
	assertCount("settlement_receivable_plan", 2)
	assertCount("settlement_receivable", 0)
	var customer, currency, contractAmount string
	var version int
	if err = db.QueryRow("SELECT customer_id,currency,contract_amount,source_contract_version FROM settlement_contract_snapshot WHERE tenant_id=?", event.TenantID).Scan(&customer, &currency, &contractAmount, &version); err != nil || customer != "42" || currency != "CNY" || contractAmount != "100.00" || version != event.Contract.Version {
		t.Fatalf("approved snapshot mismatch: customer=%s currency=%s amount=%s version=%d error=%v", customer, currency, contractAmount, version, err)
	}
	rows, err := db.Query("SELECT id,installment_no,DATE_FORMAT(due_date,'%Y-%m-%d'),planned_amount,confirmation_status,version FROM settlement_receivable_plan WHERE tenant_id=? ORDER BY installment_no", event.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	type plan struct {
		id                  string
		no                  int
		due, amount, status string
		version             int
	}
	var plans []plan
	for rows.Next() {
		var p plan
		if err = rows.Scan(&p.id, &p.no, &p.due, &p.amount, &p.status, &p.version); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, p)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for i, p := range plans {
		term := event.Contract.PaymentTerms[i]
		if p.no != term.InstallmentNo || p.due != term.DueDate || p.amount != term.Amount || p.status != "PENDING_CONFIRMATION" || p.version != 1 {
			t.Fatal("term did not reach exact pending plan")
		}
	}
	session := func(token, tenant string) string {
		t.Helper()
		cookie := "qa-finance-" + token
		oauth, _ := json.Marshal(oauth2.Token{AccessToken: token, TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
		block, err := aes.NewCipher(c.OIDCSessionKey)
		if err != nil {
			t.Fatal(err)
		}
		codec, err := cipher.NewGCM(block)
		if err != nil {
			t.Fatal(err)
		}
		nonce := make([]byte, codec.NonceSize())
		if _, err = rand.Read(nonce); err != nil {
			t.Fatal(err)
		}
		ciphertext := codec.Seal(nonce, nonce, oauth, nil)
		principal, _ := json.Marshal(service.Principal{TenantID: tenant, UserID: "qa-finance"})
		hash := sha256.Sum256([]byte(cookie))
		_, err = db.Exec(`INSERT INTO settlement_oidc_session(session_id_hash,tenant_id,identity_id,principal_json,oauth_token_ciphertext,id_token_ciphertext,authorization_revision,authorization_checked_at,token_expires_at,session_expires_at,created_at,last_seen_at) VALUES(?,?,?,?,?,?,1,UTC_TIMESTAMP(3),?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, hash[:], tenant, "qa-finance", principal, ciphertext, []byte("fixture"), time.Now().Add(time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return cookie
	}
	financeCookie := session("finance", event.TenantID)
	noPermission := session("no-permission", event.TenantID)
	foreignCookie := session("foreign", event.TenantID+"-foreign")
	path := "/api/v1/receivable-plans/" + plans[0].id + "/confirm"
	request("POST", path, []byte(`{"version":1}`), "", "", 403)
	request("POST", path, []byte(`{"version":1}`), "", noPermission, 403)
	request("POST", path, []byte(`{"version":1}`), "", foreignCookie, 409)
	assertCount("settlement_receivable", 0)
	for _, p := range plans {
		path = "/api/v1/receivable-plans/" + p.id + "/confirm"
		request("POST", path, []byte(`{"version":1}`), "", financeCookie, 200)
		request("POST", path, []byte(`{"version":1}`), "", financeCookie, 409)
	}
	assertCount("settlement_receivable", 2)
	assertCount("settlement_outbox_event", 2)
	var total, open string
	if err = db.QueryRow("SELECT SUM(original_amount),SUM(open_amount) FROM settlement_receivable WHERE tenant_id=? AND currency='CNY' AND recognition_status='CONFIRMED'", event.TenantID).Scan(&total, &open); err != nil || total != "100.00" || open != "100.00" {
		t.Fatalf("confirmed receivable sum mismatch: %s/%s error=%v", total, open, err)
	}
	request("GET", "/api/v1/receivable-plans", nil, "", financeCookie, 200)
}
