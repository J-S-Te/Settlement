package platform

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"github.com/j-s-te/settlement/internal/service"
	"golang.org/x/oauth2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type pageRefreshDB struct {
	*logoutTestDB
	principal, token []byte
}

// Connect 创建只替换 SQL 边界的隔离会话连接。
// 参数：上下文为本地测试调用。
// 返回值：会话测试连接，无错误。
func (d *pageRefreshDB) Connect(context.Context) (driver.Conn, error) {
	return &pageRefreshConn{logoutTestConn: &logoutTestConn{d.logoutTestDB}, db: d}, nil
}

type pageRefreshConn struct {
	*logoutTestConn
	db *pageRefreshDB
}

// QueryContext 返回当前 Cookie 的服务器端身份与加密令牌记录。
// 参数：查询及参数为生产 Authenticate 发出的会话读取。
// 返回值：独立会话行，不访问真实数据库。
func (c *pageRefreshConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.db.queryErr != nil {
		return nil, c.db.queryErr
	}
	return &pageRefreshRows{values: []driver.Value{"tenant-1", "identity-1", c.db.principal, c.db.token}}, nil
}

type pageRefreshRows struct {
	values []driver.Value
	done   bool
}

// Columns 返回生产会话读取所需列。
// 参数：无。
// 返回值：租户、主体、权限快照及令牌密文列。
func (*pageRefreshRows) Columns() []string {
	return []string{"tenant_id", "identity_id", "principal_json", "oauth_token_ciphertext"}
}

// Close 结束当前测试结果集。
// 参数：无。
// 返回值：无错误。
func (*pageRefreshRows) Close() error { return nil }

// Next 读取唯一一条本地会话行。
// 参数：values 接收 SQL 扫描的列值。
// 返回值：首次读取成功，随后返回 EOF。
func (r *pageRefreshRows) Next(values []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(values, r.values)
	r.done = true
	return nil
}

// TestPageRefreshOnlineAuthorization 验证结算沿用一次在线读取并传递页面刷新标记。
// 参数：t 为会话 SQL 边界和 HTTP 平台测试上下文。
// 返回值：无；重复读取、错误分类或当前授权未更新时失败。
func TestPageRefreshOnlineAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, path, marker string
		status             int
		want               error
	}{
		{"fresh roles", "/api/v1/auth/me", "1", 200, nil},
		{"removed", "/api/v1/auth/me", "1", 403, service.ErrForbidden},
		{"outage", "/api/v1/auth/me", "1", 503, ErrSessionUnavailable},
		{"database outage", "/api/v1/auth/me", "1", 200, ErrSessionUnavailable},
		{"invalid session", "/api/v1/auth/me", "1", 401, ErrUnauthenticated},
		{"ordinary read", "/api/v1/contracts", "", 200, nil},
		{"business marker ignored", "/api/v1/contracts", "1", 200, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := logoutFixture(t)
			token := &oauth2.Token{AccessToken: "valid-old-token", Expiry: time.Now().Add(time.Hour)}
			db := &pageRefreshDB{logoutTestDB: &logoutTestDB{affected: 1}, principal: mustJSON(service.Principal{TenantID: "tenant-1", IdentityID: "identity-1", PersonID: "person-1"}), token: a.encrypt(mustJSON(token))}
			if tc.name == "database outage" {
				db.queryErr = errors.New("isolated database outage")
			}
			a.db = sql.OpenDB(db)
			defer a.db.Close()
			a.cfg.OIDCEnvironmentCode = "prod"
			a.cfg.OIDCSessionIdleTTL = 30 * time.Minute
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				marker := ""
				if tc.path == "/api/v1/auth/me" && tc.marker == "1" {
					marker = "1"
				}
				if r.Header.Get("X-Authorization-Refresh") != marker {
					t.Errorf("marker=%q want=%q", r.Header.Get("X-Authorization-Refresh"), marker)
				}
				w.WriteHeader(tc.status)
				if tc.status == 200 {
					_ = json.NewEncoder(w).Encode(authorizationContext{Subject: "kc-user", SubjectID: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", ClientID: a.cfg.OIDCClientID, ApplicationCode: "settlement", EnvironmentCode: "prod", Roles: []string{"settlement_project_assistant"}, Permissions: []string{"settlement.receivable.read"}, DataScopes: []authorizationDataScope{{RoleCode: "settlement_project_assistant", ScopeType: "SELF", ScopeID: "identity-1"}}, AuthorizationRevision: 9})
				}
			}))
			defer upstream.Close()
			a.cfg.PlatformBaseURL = upstream.URL
			a.client = upstream.Client()
			request := httptest.NewRequest("GET", tc.path, nil)
			request.Header.Set("X-Authorization-Refresh", tc.marker)
			request.AddCookie(&http.Cookie{Name: a.cfg.OIDCSessionCookieName, Value: "same-cookie"})
			p, err := a.Authenticate(context.Background(), request)
			wantCalls := 1
			if tc.name == "database outage" {
				wantCalls = 0
			}
			if calls != wantCalls || !errors.Is(err, tc.want) {
				t.Fatalf("calls=%d err=%v want=%v", calls, err, tc.want)
			}
			if tc.want == nil {
				if len(db.writes) != 1 {
					t.Fatalf("authorization writes=%d want=1", len(db.writes))
				}
				fresh := tc.path == "/api/v1/auth/me" && tc.marker == "1"
				touchesIdle := strings.Contains(db.writes[0].query, "last_seen_at=UTC_TIMESTAMP(3)")
				if touchesIdle == fresh {
					t.Fatal("forced authorization refresh extended idle, or ordinary request lost legacy activity")
				}
			}
			if tc.status == 200 && tc.want == nil && (p.AuthorizationRevision != 9 || len(p.DataScopes) != 1 || p.DataScopes[0].ScopeType != "SELF") {
				t.Fatalf("old authorization survived: %+v", p)
			}
		})
	}
}
