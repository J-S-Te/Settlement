package httpapi

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
	"golang.org/x/oauth2"
)

// revisionHoldPrincipal 描述一组受控在线授权元数据，用于按真实 OIDC 会话
// 验证 HTTP 权限边界，而不是在处理器内部伪造主体。
type revisionHoldPrincipal struct {
	TenantID    string
	UserID      string
	Roles       []string
	Permissions []string
}

// revisionHoldAuthHandler 构造绑定真实 OIDC 发现与授权上下文接口的结算入口。
// 参数：t 为测试上下文，db 为脚本数据库，grant 为平台返回的在线授权。
// 返回值：handler 与携带会话的 Cookie；会话查询的脚本步骤由调用方先准备好。
func revisionHoldAuthHandler(t *testing.T, db *sql.DB, grant revisionHoldPrincipal) http.Handler {
	t.Helper()
	key := bytes.Repeat([]byte{7}, 32)
	var identity *httptest.Server
	identity = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/.well-known/openid-configuration" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                identity.URL,
				"authorization_endpoint":                identity.URL + "/authorize",
				"token_endpoint":                        identity.URL + "/token",
				"jwks_uri":                              identity.URL + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
			return
		}
		if r.URL.Path != "/oauth2/authorization-context" || r.Header.Get("Authorization") != "Bearer qa-session-token" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": "qa-user", "subject_id": grant.UserID, "identity_id": grant.UserID,
			"tenant_id": grant.TenantID, "client_id": "settlement-qa-web",
			"application_code": "settlement", "environment_code": "test",
			"roles": grant.Roles, "permissions": grant.Permissions,
			"data_scopes":                   []map[string]string{{"role_code": grant.Roles[0], "scope_type": "APPLICATION"}},
			"catalog_version":               "4",
			"compatible_catalog_versions":   []string{"4"},
			"role_config_hash":              "settlement-v4-contract-import",
			"compatible_role_config_hashes": []string{"settlement-v4-contract-import"},
			"authorization_revision":        1,
		})
	}))
	t.Cleanup(identity.Close)
	cfg := config.Config{
		PlatformBaseURL: identity.URL, OIDCIssuer: identity.URL, OIDCClientID: "settlement-qa-web",
		OIDCEnvironmentCode: "test", OIDCSessionCookieName: "settlement_session",
		OIDCSessionKey: key, OIDCSessionIdleTTL: 30 * time.Minute, OIDCSessionAbsoluteTTL: time.Hour,
		PublicOrigin: "https://settlement.test",
	}
	auth, err := platform.NewAuthenticator(context.Background(), db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return New(db, cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), auth, nil, nil)
}

// sessionStepsFor 返回一次在线会话校验所需的脚本步骤：读取并刷新会话行。
// 参数：t 为测试上下文，key 为 AES 会话密钥，grant 为会话绑定的租户与身份。
// 返回值：脚本步骤；令牌密文与生产实现同格式（nonce 前缀 + GCM）。
func sessionStepsFor(t *testing.T, key []byte, grant revisionHoldPrincipal) []scriptdb.Step {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	principalJSON, _ := json.Marshal(service.Principal{TenantID: grant.TenantID, UserID: grant.UserID, IdentityID: grant.UserID})
	tokenJSON, _ := json.Marshal(oauth2.Token{AccessToken: "qa-session-token", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
	nonce := make([]byte, codec.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return []scriptdb.Step{
		{Kind: "query", SQL: "FROM settlement_oidc_session", Rows: [][]driver.Value{{grant.TenantID, grant.UserID, principalJSON, codec.Seal(nonce, nonce, tokenJSON, nil)}}},
		{Kind: "exec", SQL: "UPDATE settlement_oidc_session SET principal_json"},
	}
}

// revisionHoldReadOnlyLicense 模拟只读期许可：历史查询放行，业务变更拒绝。
type revisionHoldReadOnlyLicense struct {
	operations []core.Operation
}

// Check 记录并判定商业许可操作。
// 参数：ctx 为上下文，operation 为本次请求的许可类别。
// 返回值：只读/基础服务放行，业务变更返回拒绝。
func (l *revisionHoldReadOnlyLicense) Check(_ context.Context, operation core.Operation) error {
	l.operations = append(l.operations, operation)
	if operation == core.MUTATE_BUSINESS {
		return errors.New("commercial license is in read-only period")
	}
	return nil
}

// revisionHoldRequest 构造带来源与CSRF头的结算写请求。
// 参数：method/path/body 为请求内容，cookie 为可选会话。
// 返回值：可直接交给 handler 的请求。
func revisionHoldRequest(method, path, body, cookie string) *http.Request {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, reader)
	r.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		r.Header.Set("Origin", "https://settlement.test")
		r.Header.Set("X-CSRF-Token", "1")
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: "settlement_session", Value: cookie})
	}
	return r
}

// TestContractRevisionHoldListRequiresPermission 验证缺少 settlement.revision.read
// 的在线会话被 403 拒绝，且拒绝发生在任何工单查询之前。
// 参数：t 为测试上下文。
// 返回值：无；非 403 或触达业务查询时失败。
func TestContractRevisionHoldListRequiresPermission(t *testing.T) {
	grant := revisionHoldPrincipal{TenantID: "tenant-a", UserID: "qa-manager", Roles: []string{"settlement_management"}, Permissions: []string{"settlement.report.read", "settlement.notification.read"}}
	steps := sessionStepsFor(t, bytes.Repeat([]byte{7}, 32), grant)
	db := scriptdb.Open(t, steps...)
	handler := revisionHoldAuthHandler(t, db, grant)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/contract-revision-holds", "", "qa-session-cookie"))
	if response.Code != http.StatusForbidden {
		t.Fatalf("缺少权限必须 403，实际=%d body=%s", response.Code, response.Body.String())
	}
}

// TestContractRevisionHoldListBindsTrustedTenantAndFilters 验证列表按认证主体租户
// 与状态/合同筛选、分页参数查询，客户端自带的 tenant_id 不能改变租户边界。
// 参数：t 为测试上下文。
// 返回值：无；租户绑定、筛选参数或响应包络不符时失败。
func TestContractRevisionHoldListBindsTrustedTenantAndFilters(t *testing.T) {
	var args []driver.NamedValue
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_revision_hold cs", Args: &args, Rows: [][]driver.Value{
		{"hold-1", "contract-1", "HT-1", "snapshot-new", "EVT-4", int64(1), int64(2), "PLAN_DIFF_IMPLEMENTED_FACTS", []byte(`{"diffs":[{"installment_no":1}]}`), "PENDING_REVIEW", "", "", "2026-10-30T08:00:00.000000Z"},
	}})
	handler := New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/contract-revision-holds?tenant_id=tenant-foreign&status=PENDING_REVIEW&contract_ref=HT-1&limit=17&offset=5", "", ""))
	if response.Code != http.StatusOK {
		t.Fatalf("列表状态=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"items":[`) || !strings.Contains(body, `"hold_reason":"PLAN_DIFF_IMPLEMENTED_FACTS"`) || !strings.Contains(body, `"limit":17`) || !strings.Contains(body, `"offset":5`) {
		t.Fatalf("列表响应=%s", body)
	}
	if len(args) != 6 || args[0].Value != "dev" || args[1].Value != "PENDING_REVIEW" || args[2].Value != "HT-1" || args[3].Value != "HT-1" || args[4].Value != int64(17) || args[5].Value != int64(5) {
		t.Fatalf("租户/筛选/分页绑定=%+v", args)
	}
}

// TestContractRevisionHoldListRejectsInvalidQuery 验证越界分页、未知状态与过长
// 合同筛选在进入数据库前返回 400。
// 参数：t 为测试上下文。
// 返回值：无；非法查询未拒绝或触达数据库时失败。
func TestContractRevisionHoldListRejectsInvalidQuery(t *testing.T) {
	for _, path := range []string{
		"/api/v1/contract-revision-holds?limit=0",
		"/api/v1/contract-revision-holds?limit=101",
		"/api/v1/contract-revision-holds?offset=100001",
		"/api/v1/contract-revision-holds?status=UNKNOWN",
		"/api/v1/contract-revision-holds?contract_ref=" + strings.Repeat("a", 129),
	} {
		handler := New(nil, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, path, "", ""))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}

// TestContractRevisionHoldReadOnlyLicenseKeepsGetAndDeniesResolve 验证只读许可期
// 历史查询仍 200，而人工关单按业务变更被 403 拒绝。
// 参数：t 为测试上下文。
// 返回值：无；只读期越过许可或误拒只读查询时失败。
func TestContractRevisionHoldReadOnlyLicenseKeepsGetAndDeniesResolve(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_revision_hold cs"})
	gate := &revisionHoldReadOnlyLicense{}
	handler := New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil, gate)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/contract-revision-holds", "", ""))
	if response.Code != http.StatusOK {
		t.Fatalf("只读期列表状态=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/hold-1/resolve", `{"reason":"已按修订金额完成调整与红冲"}`, ""))
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "COMMERCIAL_LICENSE_DENIED") {
		t.Fatalf("只读期关单必须 403，实际=%d body=%s", response.Code, response.Body.String())
	}
	if len(gate.operations) != 2 || gate.operations[0] != core.READ_HISTORY || gate.operations[1] != core.MUTATE_BUSINESS {
		t.Fatalf("许可类别=%v", gate.operations)
	}
}

// TestContractRevisionHoldResolveRequiresReason 验证 reason 必填且长度受限，
// 非法请求不进入事务。
// 参数：t 为测试上下文。
// 返回值：无；缺失或过短原因未返回 400 时失败。
func TestContractRevisionHoldResolveRequiresReason(t *testing.T) {
	handler := New(nil, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	for _, body := range []string{`{}`, `{"reason":""}`, `{"reason":"短"}`} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/hold-1/resolve", body, ""))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s 状态=%d body=%s", body, response.Code, response.Body.String())
		}
	}
}

// revisionHoldResolveDB 构造一次成功关单所需的脚本事务边界：
// 锁工单、两次快照对账状态校验、置 RESOLVED、写高风险审计并提交。
// 参数：t 为测试上下文，auditArgs 可选接收审计参数。
// 返回值：脚本数据库。
func revisionHoldResolveDB(t *testing.T, auditArgs *[]driver.NamedValue) *sql.DB {
	t.Helper()
	return scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT status,COALESCE(contract_snapshot_id,'') FROM settlement_contract_revision_hold", Rows: [][]driver.Value{{"PENDING_REVIEW", "snapshot-new"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_revision_hold SET status='RESOLVED'"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: auditArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "commit"},
	)
}

// TestContractRevisionHoldResolveWritesAudit 验证人工关单落库 RESOLVED 并写高风险审计。
// 参数：t 为测试上下文。
// 返回值：无；状态或审计缺失时失败。
func TestContractRevisionHoldResolveWritesAudit(t *testing.T) {
	var auditArgs []driver.NamedValue
	db := revisionHoldResolveDB(t, &auditArgs)
	handler := New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/hold-1/resolve", `{"reason":"已按修订金额完成调整与红冲"}`, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("关单状态=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"status":"RESOLVED"`) {
		t.Fatalf("关单响应=%s", response.Body.String())
	}
	if len(auditArgs) < 8 || auditArgs[4].Value != "SETTLEMENT_CONTRACT_REVISION_HOLD_RESOLVED" {
		t.Fatalf("关单审计=%+v", auditArgs)
	}
	payload, _ := auditArgs[7].Value.([]byte)
	if !strings.Contains(string(payload), `"risk_level":"HIGH"`) {
		t.Fatalf("审计载荷=%s", payload)
	}
}

// TestContractRevisionHoldResolveIsIdempotent 验证同原因重放返回 2xx 且不重复写审计，
// 换原因关单返回 409 而不是覆盖人工处理说明。
// 参数：t 为测试上下文。
// 返回值：无；重放语义不符时失败。
func TestContractRevisionHoldResolveIsIdempotent(t *testing.T) {
	const reason = "已按修订金额完成调整与红冲"
	replay := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT status,COALESCE(contract_snapshot_id,'') FROM settlement_contract_revision_hold", Rows: [][]driver.Value{{"RESOLVED", "snapshot-new"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT resolution_reason FROM settlement_contract_revision_hold", Rows: [][]driver.Value{{reason}}},
		scriptdb.Step{Kind: "rollback"},
	)
	handler := New(replay, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/hold-1/resolve", `{"reason":"`+reason+`"}`, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("同原因重放必须幂等成功，实际=%d body=%s", response.Code, response.Body.String())
	}
	mismatch := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT status,COALESCE(contract_snapshot_id,'') FROM settlement_contract_revision_hold", Rows: [][]driver.Value{{"RESOLVED", "snapshot-new"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT resolution_reason FROM settlement_contract_revision_hold", Rows: [][]driver.Value{{"另一处理说明"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	response = httptest.NewRecorder()
	New(mismatch, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil).ServeHTTP(response, revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/hold-1/resolve", `{"reason":"`+reason+`"}`, ""))
	if response.Code != http.StatusConflict {
		t.Fatalf("换原因关单必须 409，实际=%d body=%s", response.Code, response.Body.String())
	}
}

// TestContractRevisionHoldCrossTenantRowNotVisible 验证跨租户行不会返回：列表只
// 绑定认证主体租户，客户端无法用查询参数请求其它租户工单。
// 参数：t 为测试上下文。
// 返回值：无；查询绑定被客户端参数改写时失败。
func TestContractRevisionHoldCrossTenantRowNotVisible(t *testing.T) {
	var args []driver.NamedValue
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_revision_hold cs", Args: &args})
	handler := New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/contract-revision-holds?tenant_id=tenant-foreign", "", ""))
	if response.Code != http.StatusOK {
		t.Fatalf("列表状态=%d body=%s", response.Code, response.Body.String())
	}
	if len(args) == 0 || args[0].Value != "dev" {
		t.Fatalf("租户必须取自认证主体: %+v", args)
	}
	for _, arg := range args {
		if arg.Value == "tenant-foreign" {
			t.Fatalf("客户端租户参数不得进入查询: %+v", args)
		}
	}
}

// TestMySQLContractRevisionHoldCrossTenantAndResolve 在隔离真实库上验证跨租户
// 工单不可见、不可关单，本租户关单落库并只写一次审计（同原因重放幂等）。
// 参数：t 为测试上下文；无显式隔离 DSN 时跳过。
// 返回值：无；跨租户越界、状态或审计不符时失败。
func TestMySQLContractRevisionHoldCrossTenantAndResolve(t *testing.T) {
	db := roleSQLMySQL(t)
	ownID, foreignID := roleSQLID(t), roleSQLID(t)
	for _, row := range []struct{ id, tenant, contractNo string }{
		{ownID, "dev", "HT-OWN"},
		{foreignID, "tenant-foreign", "HT-FOREIGN"},
	} {
		if _, err := db.Exec(`INSERT INTO settlement_contract_revision_hold(id,tenant_id,source_application,source_contract_id,source_contract_no,revision_event_id,from_contract_version,to_contract_version,hold_reason,diff_json,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, row.id, row.tenant, "contract_management", "contract-"+row.id, row.contractNo, "EVT-"+row.id, 1, 2, "PLAN_DIFF_IMPLEMENTED_FACTS", []byte(`{"diffs":[]}`), "PENDING_REVIEW"); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/contract-revision-holds", "", ""))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "HT-OWN") || strings.Contains(response.Body.String(), "HT-FOREIGN") {
		t.Fatalf("跨租户列表边界: %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/"+foreignID+"/resolve", `{"reason":"跨租户关单必须不可见"}`, ""))
	if response.Code != http.StatusNotFound {
		t.Fatalf("跨租户关单必须 404，实际=%d body=%s", response.Code, response.Body.String())
	}
	body := `{"reason":"已按修订金额完成调整与红冲"}`
	for attempt := 0; attempt < 2; attempt++ {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/"+ownID+"/resolve", body, ""))
		if response.Code != http.StatusOK {
			t.Fatalf("第%d次关单状态=%d body=%s", attempt+1, response.Code, response.Body.String())
		}
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM settlement_contract_revision_hold WHERE id=? AND tenant_id='dev'`, ownID).Scan(&status); err != nil || status != "RESOLVED" {
		t.Fatalf("关单落库 status=%s err=%v", status, err)
	}
	var audits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_audit_event WHERE tenant_id='dev' AND resource_id=? AND action='SETTLEMENT_CONTRACT_REVISION_HOLD_RESOLVED'`, ownID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("关单审计 count=%d err=%v", audits, err)
	}
}

// TestContractRevisionHoldUsesDedicatedPermissions 验证 HTTP 与 service 使用同一
// 专用权限：仅 settlement.revision.read 的会话可列表，仅 settlement.revision.resolve
// 的会话可关单，两者都无的会话两条路径均 403。
// 参数：t 为测试上下文。
// 返回值：无；仍要求旧应收权限或放行无权限会话时失败。
func TestContractRevisionHoldUsesDedicatedPermissions(t *testing.T) {
	t.Run("list-with-revision-read-only", func(t *testing.T) {
		grant := revisionHoldPrincipal{TenantID: "tenant-a", UserID: "qa-sales", Roles: []string{"settlement_sales"}, Permissions: []string{"settlement.revision.read"}}
		var args []driver.NamedValue
		steps := append(sessionStepsFor(t, bytes.Repeat([]byte{7}, 32), grant), scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_revision_hold cs", Args: &args})
		handler := revisionHoldAuthHandler(t, scriptdb.Open(t, steps...), grant)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/contract-revision-holds", "", "qa-session-cookie"))
		if response.Code != http.StatusOK {
			t.Fatalf("只有 revision.read 必须能列表，实际=%d body=%s", response.Code, response.Body.String())
		}
		// 销售仍按合同责任范围过滤：租户后紧跟责任人参数。
		if len(args) < 2 || args[0].Value != "tenant-a" || args[1].Value != "qa-sales" {
			t.Fatalf("责任范围绑定=%+v", args)
		}
	})
	t.Run("resolve-with-revision-resolve-only", func(t *testing.T) {
		grant := revisionHoldPrincipal{TenantID: "tenant-a", UserID: "qa-manager", Roles: []string{"settlement_finance_manager"}, Permissions: []string{"settlement.revision.resolve"}}
		steps := append(sessionStepsFor(t, bytes.Repeat([]byte{7}, 32), grant), scriptdb.Step{Kind: "begin"},
			scriptdb.Step{Kind: "query", SQL: "SELECT status,COALESCE(contract_snapshot_id,'') FROM settlement_contract_revision_hold", Rows: [][]driver.Value{{"PENDING_REVIEW", "snapshot-new"}}},
			scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
			scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
			scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_revision_hold SET status='RESOLVED'"},
			scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
			scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
			scriptdb.Step{Kind: "commit"},
		)
		handler := revisionHoldAuthHandler(t, scriptdb.Open(t, steps...), grant)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/hold-1/resolve", `{"reason":"已按修订金额完成调整与红冲"}`, "qa-session-cookie"))
		if response.Code != http.StatusOK {
			t.Fatalf("只有 revision.resolve 必须能关单，实际=%d body=%s", response.Code, response.Body.String())
		}
	})
	t.Run("no-revision-permission-denied", func(t *testing.T) {
		grant := revisionHoldPrincipal{TenantID: "tenant-a", UserID: "qa-management", Roles: []string{"settlement_management"}, Permissions: []string{"settlement.report.read", "settlement.notification.read"}}
		for _, request := range []*http.Request{
			revisionHoldRequest(http.MethodGet, "/api/v1/contract-revision-holds", "", "qa-session-cookie"),
			revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/hold-1/resolve", `{"reason":"已按修订金额完成调整与红冲"}`, "qa-session-cookie"),
		} {
			handler := revisionHoldAuthHandler(t, scriptdb.Open(t, sessionStepsFor(t, bytes.Repeat([]byte{7}, 32), grant)...), grant)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s %s 必须 403，实际=%d body=%s", request.Method, request.URL.Path, response.Code, response.Body.String())
			}
		}
	})
}

// TestContractRevisionHoldResolveReasonRuneBounds 验证处理说明按字符 5–500 校验：
// 3 个汉字 + 1 个字母（4 字、10 字节）拒绝，5 个汉字与 500 字通过，501 字拒绝。
// 参数：t 为测试上下文。
// 返回值：无；按字节判或边界不符时失败。
func TestContractRevisionHoldResolveReasonRuneBounds(t *testing.T) {
	for _, body := range []string{
		`{"reason":"已按修a"}`,
		`{"reason":"` + strings.Repeat("字", 501) + `"}`,
	} {
		handler := New(nil, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/hold-1/resolve", body, ""))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("reason 长度越界必须 400，实际=%d body=%s", response.Code, response.Body.String())
		}
	}
	for _, reason := range []string{"已按修订金额", strings.Repeat("字", 500)} {
		handler := New(revisionHoldResolveDB(t, nil), config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, revisionHoldRequest(http.MethodPost, "/api/v1/contract-revision-holds/hold-1/resolve", `{"reason":"`+reason+`"}`, ""))
		if response.Code != http.StatusOK {
			t.Fatalf("reason=%d 字必须通过，实际=%d body=%s", len([]rune(reason)), response.Code, response.Body.String())
		}
	}
}
