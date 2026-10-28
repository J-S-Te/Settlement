package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/migration"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/migrations"
)

// roleSQLID 生成仅用于隔离测试的稳定长度随机标识。
// 参数：t 为测试上下文。
// 返回值：32位十六进制测试标识。
func roleSQLID(t *testing.T) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw[:])
}

// roleSQLMySQL 仅在显式隔离DSN下新建随机schema并应用真实全部迁移。
// 参数：t 为测试上下文，环境DSN必须为回环settlement_e2e命名库。
// 返回值：绑定自有schema的连接；清理只删除本次新建schema。
func roleSQLMySQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_TEST_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_TEST_DSN not configured; real HTTP SQL acceptance not executed")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !regexp.MustCompile(`^settlement_e2e(_[a-z0-9_]+)?$`).MatchString(cfg.DBName) || cfg.Net != "tcp" || !(strings.HasPrefix(cfg.Addr, "127.0.0.1:") || strings.HasPrefix(cfg.Addr, "localhost:")) {
		t.Fatal("HTTP SQL test requires explicit loopback settlement_e2e DSN; DSN withheld")
	}
	name := "settlement_e2e_role_http_" + roleSQLID(t)
	controlCfg := cfg.Clone()
	controlCfg.DBName = ""
	control, err := sql.Open("mysql", controlCfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated control failed; DSN withheld")
	}
	t.Cleanup(func() { _ = control.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err = control.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create owned HTTP SQL schema failed; DSN withheld")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := control.ExecContext(ctx, "DROP DATABASE `"+name+"`"); err != nil {
			t.Error("drop owned HTTP SQL schema failed; DSN withheld")
		}
	})
	runCfg := cfg.Clone()
	runCfg.DBName = name
	runCfg.ParseTime = true
	runCfg.MultiStatements = true
	if err = migration.Run(ctx, runCfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatalf("apply real HTTP SQL migrations: %v", err)
	}
	db, err := sql.Open("mysql", runCfg.FormatDSN())
	if err != nil {
		t.Fatal("open owned HTTP SQL schema failed; DSN withheld")
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// roleSQLGet 通过真实handler执行组件GET并校验200及JSON包络。
// 参数：t 为测试上下文，handler 为真实结算入口，path 为目标读取路径。
// 返回值：JSON原始data内容；失败会直接停止当前子测试。
func roleSQLGet(t *testing.T, handler http.Handler, path string) json.RawMessage {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s HTTP=%d body=%s", path, response.Code, response.Body.String())
	}
	var result struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Data == nil {
		t.Fatalf("GET %s invalid JSON data: %s", path, response.Body.String())
	}
	return result.Data
}

// roleSQLSnapshot 建立真实同租户合同快照，不模拟数据库返回。
// 参数：t 为测试上下文，db 为独占schema，tenant/contract/no 为测试业务标识。
// 返回值：真实写入的合同快照ID。
func roleSQLSnapshot(t *testing.T, db *sql.DB, tenant, contract, no string) string {
	t.Helper()
	id := roleSQLID(t)
	_, err := db.Exec(`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,'contract_management',?,?,?,1,'customer','隔离客户','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, roleSQLID(t), contract, no)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestMySQLRoleHTTPReadQueriesOnEmptySchema 验证新增及旧读取接口的真实SQL接线。
// 参数：t 为测试上下文；DevelopmentAuth只用于HTTP组件，不验证OIDC身份。
// 返回值：无；每个路径必须真实查询成功并返回200。
func TestMySQLRoleHTTPReadQueriesOnEmptySchema(t *testing.T) {
	db := roleSQLMySQL(t)
	handler := New(db, config.Config{DevelopmentAuth: true}, nil, nil, nil, nil)
	for _, path := range []string{"receivable-plans", "receivables", "invoice-requests", "tax-invoices", "dunning/cases", "dunning/actions", "reports/aging", "settings", "receipt-differences", "accounting-vouchers", "contract-references", "project-assignments"} {
		t.Run(path, func(t *testing.T) { roleSQLGet(t, handler, "/api/v1/"+path) })
	}
}

// TestMySQLRoleHTTPInvoicePrivacyAndPendingDunning 验证冻结审批字段、税号策略和待确认节点均经真实SQL返回。
// 参数：t 为测试上下文；所有种子只写入本次随机独占schema。
// 返回值：无；任意字段泄漏或节点丢失均失败。
func TestMySQLRoleHTTPInvoicePrivacyAndPendingDunning(t *testing.T) {
	db := roleSQLMySQL(t)
	snapshot := roleSQLSnapshot(t, db, "dev", "contract", "HT-SQL")
	request, invoice, plan, policy, caseID, action := roleSQLID(t), roleSQLID(t), roleSQLID(t), roleSQLID(t), roleSQLID(t), roleSQLID(t)
	taxNo := "91310000PRIVATE12345"
	buyer, _ := json.Marshal(map[string]string{"name": "隔离客户", "tax_no": taxNo})
	steps := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO settlement_settings(tenant_id,settings_json,version) VALUES('dev','{"field_policies":{"tax_number_visible":false}}',9)`, nil},
		{`INSERT INTO settlement_invoice_request(id,tenant_id,request_no,contract_snapshot_id,buyer_profile_snapshot,invoice_type,amount_excl_tax,tax_amount,amount_incl_tax,status,idempotency_key,submitted_by,created_at,updated_at) VALUES(?,'dev','IR-SQL',?,?,'VAT_NORMAL',100,0,100,'ISSUED','invoice-key','applicant',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{request, snapshot, buyer}},
		{`INSERT INTO settlement_invoice_special_review(tenant_id,invoice_request_id,special_reason,threshold_snapshot,settings_version,required_review,status) VALUES('dev',?,'冻结特殊事项','5000.00',7,TRUE,'APPROVED')`, []any{request}},
		{`INSERT INTO settlement_tax_invoice(id,tenant_id,invoice_request_id,invoice_code,invoice_no,invoice_type,currency,amount_excl_tax,tax_amount,amount_incl_tax,issue_date,buyer_profile_snapshot,seller_profile_snapshot,status,issued_by_channel,created_at) VALUES(?,'dev',?,'CODE','NO-SQL','VAT_NORMAL','CNY',100,0,100,CURDATE(),?,'{}','ISSUED','MANUAL',UTC_TIMESTAMP(3))`, []any{invoice, request, buyer}},
		{`INSERT INTO settlement_tax_invoice_item(id,tenant_id,tax_invoice_id,line_no,item_name,tax_classification_code,quantity,unit_price_excl_tax,amount_excl_tax,tax_rate,tax_amount,amount_incl_tax) VALUES(?,'dev',?,1,'服务','301',1,100,100,0,0,100)`, []any{roleSQLID(t), invoice}},
		{`INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,created_at,updated_at) VALUES(?,'dev',?,1,CURDATE(),100,'CONTRACT_EVENT','PENDING_CONFIRMATION',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{plan, snapshot}},
		{`INSERT INTO settlement_dunning_policy(id,tenant_id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled,created_at,updated_at) VALUES(?,'dev','pending-plan',1,0,30,'PLAN_CONFIRM','CONTRACT_OWNER','IN_APP',7,'NORMAL',TRUE,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{policy}},
		{`INSERT INTO settlement_dunning_case(id,tenant_id,receivable_id,plan_id,dunning_policy_id,policy_version,status,next_action_at,created_at,updated_at) VALUES(?,'dev',NULL,?,?,1,'OPEN',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{caseID, plan, policy}},
		{`INSERT INTO settlement_dunning_action(id,tenant_id,dunning_case_id,action_type,recipient_user_id,channel,priority,event_id,status,created_at) VALUES(?,'dev',?,'PLAN_CONFIRM','dev-finance','IN_APP','NORMAL',?,'PENDING',UTC_TIMESTAMP(3))`, []any{action, caseID, roleSQLID(t)}},
	}
	for _, step := range steps {
		if _, err := db.Exec(step.query, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(db, config.Config{DevelopmentAuth: true}, nil, nil, nil, nil)
	for _, path := range []string{"/api/v1/tax-invoices", "/api/v1/tax-invoices/" + invoice} {
		data := roleSQLGet(t, handler, path)
		if strings.Contains(string(data), taxNo) || strings.Contains(string(data), "913100") {
			t.Fatalf("税号策略未脱敏：%s", data)
		}
		if strings.Contains(path, invoice) {
			var detail map[string]any
			if err := json.Unmarshal(data, &detail); err != nil {
				t.Fatal(err)
			}
			if detail["buyer_tax_no_masked"] != "" {
				t.Fatalf("禁显税号仍返回片段：%s", data)
			}
		} else {
			var list []map[string]any
			if err := json.Unmarshal(data, &list); err != nil || len(list) != 1 {
				t.Fatalf("发票列表错误：%s", data)
			}
			if list[0]["buyer_tax_no_masked"] != "" {
				t.Fatalf("禁显税号仍返回片段：%s", data)
			}
		}
	}
	var requests []struct {
		SpecialReason string `json:"special_reason"`
		Threshold     string `json:"threshold_snapshot"`
		Version       int    `json:"settings_version"`
	}
	data := roleSQLGet(t, handler, "/api/v1/invoice-requests")
	if err := json.Unmarshal(data, &requests); err != nil || len(requests) != 1 || requests[0].SpecialReason != "冻结特殊事项" || requests[0].Threshold != "5000.00" || requests[0].Version != 7 {
		t.Fatalf("冻结审批字段丢失或使用当前规则：%s", data)
	}
	for _, target := range []struct{ path, id string }{{"/api/v1/dunning/cases", caseID}, {"/api/v1/dunning/actions", action}} {
		var list []map[string]any
		data := roleSQLGet(t, handler, target.path)
		if err := json.Unmarshal(data, &list); err != nil || len(list) != 1 || list[0]["id"] != target.id || list[0]["receivable_no"] != "PLAN-1" {
			t.Fatalf("待确认节点被应收内连接丢弃：%s", data)
		}
	}
}

// TestMySQLScopedQueryFiltersBeforePagination 验证SQL分页前过滤归属且混合复核角色不扩大开票范围。
// 参数：t 为测试上下文，真实主体仅通过service.Principal明确构造，不声称真实身份验签。
// 返回值：无；第一页只能包含本人合同，后续页不能泄漏他人或其他租户。
func TestMySQLScopedQueryFiltersBeforePagination(t *testing.T) {
	db := roleSQLMySQL(t)
	other := roleSQLSnapshot(t, db, "dev", "other", "01-OTHER")
	own := roleSQLSnapshot(t, db, "dev", "own", "02-OWN")
	_ = roleSQLSnapshot(t, db, "other-tenant", "cross", "00-CROSS")
	for _, row := range []struct{ contract, owner string }{{"other", "other-user"}, {"own", "sales-user"}} {
		if _, err := db.Exec(`INSERT INTO settlement_contract_responsibility(tenant_id,source_contract_id,owner_user_id,version,active,updated_at) VALUES('dev',?,?,1,TRUE,UTC_TIMESTAMP(3))`, row.contract, row.owner); err != nil {
			t.Fatal(err)
		}
	}
	api := &API{service: &service.Service{DB: db}}
	cases := []struct {
		name, permission string
		roles            []string
	}{{"sales_read", "settlement.receivable.read", []string{"settlement_sales"}}, {"sales_and_reviewer_request", "settlement.invoice.request", []string{"settlement_sales", "settlement_reviewer"}}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := service.Principal{TenantID: "dev", UserID: "sales-user", CatalogVersion: "3", Roles: tc.roles, Permissions: map[string]bool{tc.permission: true}, DataScopes: []service.DataScope{{RoleCode: "settlement_reviewer", ScopeType: "APPLICATION"}}}
			for _, offset := range []int{0, 1} {
				rows, err := api.queryScoped(context.Background(), p, tc.permission, `SELECT cs.id FROM settlement_contract_snapshot cs WHERE cs.tenant_id=? ORDER BY cs.source_contract_no LIMIT ? OFFSET ?`, "dev", 1, offset)
				if err != nil {
					t.Fatal(err)
				}
				var ids []string
				for rows.Next() {
					var id string
					if err = rows.Scan(&id); err != nil {
						rows.Close()
						t.Fatal(err)
					}
					ids = append(ids, id)
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					t.Fatal(err)
				}
				if offset == 0 && (len(ids) != 1 || ids[0] != own) || offset == 1 && len(ids) != 0 {
					t.Fatalf("分页范围错误：offset%d ids=%v 本人%s 他人%s", offset, ids, own, other)
				}
			}
		})
	}
}
