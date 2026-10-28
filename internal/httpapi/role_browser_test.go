package httpapi

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/authz"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/projectreference"
	"github.com/j-s-te/settlement/internal/service"
	"golang.org/x/oauth2"
)

type roleBrowserDirectory struct{}

// TestMySQLRoleSettingsPersistence 检查设置首次保存、连续修改与乐观锁冲突的真实持久化。
// 参数：t 为测试上下文，仅使用显式隔离DSN。
// 返回值：无；保存、版本或审计缺失均失败。
func TestMySQLRoleSettingsPersistence(t *testing.T) {
	db := roleSQLMySQL(t)
	svc := service.Service{DB: db}
	p := service.Principal{TenantID: "qa-role-settings", UserID: "manager-user", Roles: []string{"settlement_finance_manager"}, Permissions: map[string]bool{"settlement.settings.manage": true}, CatalogVersion: "3", DataScopes: []service.DataScope{{RoleCode: "settlement_finance_manager", ScopeType: "APPLICATION"}}}
	ctx := context.Background()
	in, err := svc.GetSettlementSettings(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	in.ReminderLeadDays = 8
	first, err := svc.PutSettlementSettings(ctx, p, in)
	if err != nil {
		t.Fatalf("first settings save: %v", err)
	}
	if first.Version != 1 {
		t.Fatalf("first version = %d", first.Version)
	}
	if _, err = svc.PutSettlementSettings(ctx, p, in); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("duplicate initial settings save: %v", err)
	}
	first.ReminderLeadDays = 9
	second, err := svc.PutSettlementSettings(ctx, p, first)
	if err != nil || second.Version != 2 {
		t.Fatalf("repeat settings save: version=%d err=%v", second.Version, err)
	}
	if _, err = svc.PutSettlementSettings(ctx, p, first); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("stale settings version: %v", err)
	}
	var audits int
	if err = db.QueryRow(`SELECT COUNT(*) FROM settlement_audit_event WHERE tenant_id=? AND action='SETTLEMENT_SETTINGS_CHANGED'`, p.TenantID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("settings audit count=%d err=%v", audits, err)
	}
}

// ContainsRole 核验明确测试夹具中的有效助理身份。
// 参数：ctx 为上下文，user 为虚构用户，role 为待核验角色。
// 返回值：夹具成员是否有效；不连接真实人员目录。
func (roleBrowserDirectory) ContainsRole(ctx context.Context, user, role string) (bool, error) {
	return role == "settlement_project_assistant" && (user == "assistant-user" || user == "assistant-two"), nil
}

// ListRole 返回夹具内满足关键词的有效助理候选。
// 参数：ctx 为上下文，role 为角色，keyword 为关键词，page/size 为分页。
// 返回值：虚构人员与总数；不代表真实平台目录联调。
func (d roleBrowserDirectory) ListRole(ctx context.Context, role, keyword string, page, size int) ([]platform.Personnel, int64, error) {
	items := []platform.Personnel{}
	for _, user := range []string{"assistant-user", "assistant-two"} {
		if ok, _ := d.ContainsRole(ctx, user, role); ok && (keyword == "" || strings.Contains(user, keyword)) {
			items = append(items, platform.Personnel{UserID: user, DisplayName: user, Roles: []string{role}})
		}
	}
	return items, int64(len(items)), nil
}

// ListCollectors 实现组件所需目录接口且不额外授予催收身份。
// 参数：ctx 为上下文，keyword 为关键词，page/size 为分页。
// 返回值：空催收目录和零总数。
func (roleBrowserDirectory) ListCollectors(ctx context.Context, keyword string, page, size int) ([]platform.Personnel, int64, error) {
	return []platform.Personnel{}, 0, nil
}

// ContainsCollector 拒绝夹具外催收身份。
// 参数：ctx 为上下文，user 为待核验用户。
// 返回值：恒为未配置；不会冒充真实目录查询成功。
func (roleBrowserDirectory) ContainsCollector(ctx context.Context, user string) (bool, error) {
	return false, nil
}

type roleBrowserProjects struct{}

// Resolve 返回明确边界内的项目服务测试引用。
// 参数：ctx 为上下文，tenant/project 为待解析租户与项目。
// 返回值：虚构的有效引用；未知项目返回不存在错误。
func (roleBrowserProjects) Resolve(ctx context.Context, tenant, project string) (projectreference.Reference, error) {
	if tenant != "qa-role-ui" || (project != "qa-project" && project != "qa-project-two") {
		return projectreference.Reference{}, service.ErrNotFound
	}
	return projectreference.Reference{TenantID: tenant, ProjectID: project, ContractID: "qa-contract", CustomerID: "customer", Name: project, Version: 1, Active: true}, nil
}

// roleBrowserSeed 将浏览器业务种子写入本次独占且已迁移的真实数据库。
// 参数：t 为测试上下文，db 为随机隔离schema。
// 返回值：传给浏览器的业务ID；数据不含客户真实信息。
func roleBrowserSeed(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	data := map[string]string{"snapshot_id": roleSQLSnapshot(t, db, "qa-role-ui", "qa-contract", "QA-UI-001"), "plan_id": roleSQLID(t), "receivable_id": roleSQLID(t), "receipt_id": roleSQLID(t), "allocation_id": roleSQLID(t), "project_id": "qa-project", "project2_id": "qa-project-two", "assistant_id": "assistant-user", "assistant2_id": "assistant-two"}
	steps := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO settlement_contract_responsibility VALUES('qa-role-ui','qa-contract','sales-user',1,TRUE,UTC_TIMESTAMP(3))`, nil},
		{`INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,version,created_at,updated_at) VALUES(?,'qa-role-ui',?,1,'2099-12-01',100,'CONTRACT_EVENT','CONFIRMED',2,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{data["plan_id"], data["snapshot_id"]}},
		{`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,receivable_plan_id,due_date,original_amount,received_allocated_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,'qa-role-ui','QA-RV-001',?,?,'2099-12-01',100,10,90,'CNY','CONFIRMED','PARTIALLY_SETTLED','NOT_INVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{data["receivable_id"], data["snapshot_id"], data["plan_id"]}},
		{`INSERT INTO settlement_receipt(id,tenant_id,receipt_no,customer_id,customer_name_snapshot,amount,unallocated_amount,currency,receipt_date,payment_method,bank_transaction_reference,source_type,status,idempotency_key,created_at,updated_at) VALUES(?,'qa-role-ui','QA-RC-001','customer','隔离客户',20,10,'CNY',CURDATE(),'BANK_TRANSFER','QA-BANK-001','MANUAL','PARTIALLY_ALLOCATED','qa-receipt',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{data["receipt_id"]}},
		{`INSERT INTO settlement_receipt_allocation(id,tenant_id,allocation_no,receipt_id,receivable_id,allocated_amount,currency,status,match_mode,confirmed_by,confirmed_at,idempotency_key,created_at) VALUES(?,'qa-role-ui','QA-AL-001',?,?,10,'CNY','CONFIRMED','MANUAL','accountant-user',UTC_TIMESTAMP(3),'qa-allocation',UTC_TIMESTAMP(3))`, []any{data["allocation_id"], data["receipt_id"], data["receivable_id"]}},
		{`INSERT INTO settlement_project_assignment VALUES('qa-role-ui','qa-project','assistant-user',1,TRUE,'',UTC_TIMESTAMP(3),'manager-user')`, nil},
	}
	for _, step := range steps {
		if _, err := db.Exec(step.query, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range []string{"qa-project", "qa-project-two"} {
		if _, err := db.Exec(`INSERT INTO settlement_project_reference VALUES('qa-role-ui',?,'qa-contract','customer',?,1,TRUE,UTC_TIMESTAMP(3))`, project, project); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

// roleBrowserSessions 创建真实AES会话并在内存登记授权服务器测试响应。
// 参数：t 为测试上下文，db 为隔离库，key 为临时加密密钥，tokens 为授权夹具映射。
// 返回值：仅经子进程环境传递的不透明会话；不会写入报告或日志。
func roleBrowserSessions(t *testing.T, db *sql.DB, key []byte, tokens map[string]map[string]any) map[string]any {
	t.Helper()
	var manifest struct {
		Roles []struct {
			Code        string   `json:"code"`
			Permissions []string `json:"permissions"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(authz.PermissionManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	grants := map[string][]string{}
	for _, role := range manifest.Roles {
		grants[role.Code] = role.Permissions
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{}
	roles := map[string]string{"sales": "settlement_sales", "manager": "settlement_finance_manager", "accountant": "settlement_accountant", "director": "settlement_sales_director", "assistant": "settlement_project_assistant", "management": "settlement_management", "configadmin": "settlement_system_admin", "none": ""}
	for alias, role := range roles {
		user, cookie, token := alias+"-user", roleSQLID(t), roleSQLID(t)
		permissions, assigned, scopes := []string{}, []string{}, []map[string]string{}
		if role != "" {
			if len(grants[role]) == 0 {
				t.Fatalf("unknown fixture role %s", role)
			}
			permissions, assigned = grants[role], []string{role}
			scopes = append(scopes, map[string]string{"role_code": role, "scope_type": "APPLICATION"})
		}
		tokens[token] = map[string]any{"sub": "fixture-" + user, "subject_id": user, "identity_id": user, "tenant_id": "qa-role-ui", "client_id": "settlement-browser-test", "application_code": "settlement", "environment_code": "test", "roles": assigned, "permissions": permissions, "data_scopes": scopes, "catalog_version": "3", "compatible_catalog_versions": []string{"3"}, "role_config_hash": "settlement-v3-business-roles-scopes", "compatible_role_config_hashes": []string{"settlement-v3-business-roles-scopes"}, "authorization_revision": 1}
		now := time.Now().UTC()
		principal, _ := json.Marshal(service.Principal{TenantID: "qa-role-ui", UserID: user, IdentityID: user, Username: user, DisplayName: alias})
		payload, _ := json.Marshal(oauth2.Token{AccessToken: token, Expiry: now.Add(time.Hour)})
		nonce := make([]byte, codec.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(cookie))
		_, err := db.Exec(`INSERT INTO settlement_oidc_session(session_id_hash,tenant_id,identity_id,principal_json,oauth_token_ciphertext,id_token_ciphertext,authorization_revision,authorization_checked_at,token_expires_at,session_expires_at,created_at,last_seen_at) VALUES(?,'qa-role-ui',?,?,?, ?,1,?,?,?,?,?)`, digest[:], user, principal, codec.Seal(nonce, nonce, payload, nil), []byte{}, now, now.Add(time.Hour), now.Add(time.Hour), now, now)
		if err != nil {
			t.Fatal(err)
		}
		result[alias] = map[string]string{"cookie": cookie, "username": user, "user_id": user}
	}
	return result
}

// TestMySQLRoleBrowserWorkflows 验证真实Vue、浏览器、HTTP授权与SQL写入的贯通。
// 参数：t 为测试上下文；必须显式启用浏览器和隔离测试DSN。
// 返回值：无；任意浏览器断言失败导致测试失败，真实IdP与许可边界不在此组件范围。
func TestMySQLRoleBrowserWorkflows(t *testing.T) {
	if os.Getenv("SETTLEMENT_ROLE_BROWSER") != "1" {
		t.Skip("explicit isolated browser run required")
	}
	db := roleSQLMySQL(t)
	data, tokens, key := roleBrowserSeed(t, db), map[string]map[string]any{}, make([]byte, 32)
	if os.Getenv("SETTLEMENT_ROLE_LANDING") == "1" {
		roleLandingSeed(t, db, data)
	}
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sessions := roleBrowserSessions(t, db, key, tokens)
	var identity *httptest.Server
	identity = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/.well-known/openid-configuration" {
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": identity.URL, "authorization_endpoint": identity.URL + "/authorize", "token_endpoint": identity.URL + "/token", "jwks_uri": identity.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
			return
		}
		value, ok := tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if r.URL.Path != "/oauth2/authorization-context" || !ok {
			http.Error(w, "denied", 403)
			return
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer identity.Close()
	cfg := config.Config{PublicOrigin: "http://127.0.0.1:18173", PlatformBaseURL: identity.URL, OIDCIssuer: identity.URL, OIDCClientID: "settlement-browser-test", OIDCEnvironmentCode: "test", OIDCSessionCookieName: "settlement_session", OIDCSessionKey: key, OIDCSessionIdleTTL: 30 * time.Minute}
	auth, err := platform.NewAuthenticator(context.Background(), db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(New(db, cfg, nil, auth, nil, nil, roleBrowserDirectory{}, roleBrowserProjects{}))
	defer api.Close()
	roleBrowserHTTPAssertions(t, api.URL, sessions)
	if data["landing"] == "1" {
		roleLandingHTTPAssertions(t, db, api.URL, sessions, data)
	}
	fixture, err := json.Marshal(map[string]any{"origin": cfg.PublicOrigin, "apiBase": api.URL, "tenant": "qa-role-ui", "sessions": sessions, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", "src/test/ui-harness/settlement-role-browser.mjs")
	command.Dir = filepath.Join("..", "..", "..", "frontend")
	command.Env = append(os.Environ(), "SETTLEMENT_ROLE_BROWSER_FIXTURE="+string(fixture))
	output, err := command.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("isolated browser workflow failed: %v", err)
	}
	if os.Getenv("SETTLEMENT_ROLE_BROWSER_CASES") == "" {
		roleBrowserDatabaseAssertions(t, db, data)
		if data["landing"] == "1" {
			roleLandingDatabaseAssertions(t, db, data)
		}
	}
}

// roleBrowserHTTPAssertions 使用真实会话核对服务端角色权限和跨来源写入边界。
// 参数：t 为测试上下文，base 为独占HTTP服务，sessions 为内存会话。
// 返回值：无；未认证、无权限或跨来源请求未被拒绝即失败。
func roleBrowserHTTPAssertions(t *testing.T, base string, sessions map[string]any) {
	t.Helper()
	cases := []struct {
		alias, method, path, origin string
		want                        int
	}{
		{"", "GET", "settings", "", http.StatusUnauthorized},
		{"sales", "GET", "settings", "", http.StatusForbidden},
		{"none", "GET", "receivable-plans", "", http.StatusForbidden},
		{"management", "GET", "accounting-vouchers", "", http.StatusForbidden},
		{"configadmin", "GET", "receivable-plans", "", http.StatusForbidden},
		{"assistant", "POST", "project-assignments", "http://127.0.0.1:18173", http.StatusForbidden},
		{"manager", "PUT", "settings", "http://untrusted.invalid", http.StatusForbidden},
	}
	client := &http.Client{Timeout: 10 * time.Second}
	for _, item := range cases {
		request, err := http.NewRequest(item.method, base+"/api/v1/"+item.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if item.origin != "" {
			request.Header.Set("Origin", item.origin)
		}
		if item.alias != "" {
			request.AddCookie(&http.Cookie{Name: "settlement_session", Value: sessions[item.alias].(map[string]string)["cookie"]})
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("isolated HTTP boundary request failed")
		}
		response.Body.Close()
		if response.StatusCode != item.want {
			t.Fatalf("role boundary %s %s %s got %d want %d", item.alias, item.method, item.path, response.StatusCode, item.want)
		}
	}
	t.Log("real HTTP role and origin boundary assertions: 7 PASS")
}

// roleBrowserDatabaseAssertions 独立核对浏览器表单在真实数据库的最终结果。
// 参数：t 为测试上下文，db 为独占schema，data 为种子标识。
// 返回值：无；金额、版本、审批或幂等结果不符即失败。
func roleBrowserDatabaseAssertions(t *testing.T, db *sql.DB, data map[string]string) {
	t.Helper()
	checks := []struct {
		query string
		args  []any
		want  string
	}{
		{`SELECT CONCAT(planned_amount,':',version) FROM settlement_receivable_plan WHERE id=?`, []any{data["plan_id"]}, "80.00:4"},
		{`SELECT CONCAT(original_amount,':',open_amount) FROM settlement_receivable WHERE id=?`, []any{data["receivable_id"]}, "80.00:70.00"},
		{`SELECT CONCAT(version,':',active) FROM settlement_project_assignment WHERE tenant_id='qa-role-ui' AND project_id='qa-project-two' AND user_id='assistant-two'`, nil, "2:1"},
		{`SELECT note FROM settlement_project_assignment WHERE tenant_id='qa-role-ui' AND project_id='qa-project' AND user_id='assistant-user'`, nil, "隔离浏览器映射说明"},
		{`SELECT CONCAT(COUNT(*),':',MAX(status)) FROM settlement_accounting_voucher WHERE tenant_id='qa-role-ui' AND writeoff_id=?`, []any{data["allocation_id"]}, "1:NOT_CONFIGURED"},
		{`SELECT CONCAT(status,':',resolution) FROM settlement_receipt_difference WHERE tenant_id='qa-role-ui' AND receipt_id=? AND reason='隔离浏览器不明款'`, []any{data["receipt_id"]}, "REVIEW_PENDING:CARRY_FORWARD"},
	}
	for i, check := range checks {
		var got string
		if err := db.QueryRow(check.query, check.args...).Scan(&got); err != nil {
			t.Fatalf("database assertion %d: %v", i, err)
		}
		if got != check.want {
			t.Fatalf("database assertion %d got %s want %s", i, got, check.want)
		}
	}
	fmt.Println("browser database postconditions: 6 PASS")
}
