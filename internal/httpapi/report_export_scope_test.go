package httpapi

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// reportScopeGrant 返回报表入口的在线授权：财务经理角色目录内同时具备报表读取与导出能力，可由
// scopeType 控制是否受合同/项目范围限制。
// 参数：scopeType 为平台返回的数据范围类型。
// 返回值：可直接交给真实 OIDC 会话测试基建的授权元数据。
func reportScopeGrant(scopeType string) revisionHoldPrincipal {
	return revisionHoldPrincipal{
		TenantID:    "tenant-report",
		UserID:      "qa-finance",
		Roles:       []string{"settlement_finance_manager"},
		Permissions: []string{"settlement.report.read", "settlement.report.export"},
		ScopeType:   scopeType,
	}
}

// TestReportEndpointsRejectRestrictedDataScope 验证 M-5：数据范围受限的主体即使持有报表读取/导出
// 权限，也被 7 个报表入口统一 403，且拒绝发生在任何业务查询之前。
// 参数：t 为测试上下文。
// 返回值：无；任一入口非 403、错误码不符或触达业务SQL时失败。
func TestReportEndpointsRejectRestrictedDataScope(t *testing.T) {
	for _, endpoint := range []struct {
		name, method, path string
	}{
		{"dashboard", http.MethodGet, "/api/v1/dashboard"},
		{"aging-summary", http.MethodGet, "/api/v1/reports/aging"},
		{"invoiced-top10", http.MethodGet, "/api/v1/reports/invoiced-receivables-top10"},
		{"create-invoiced-export", http.MethodPost, "/api/v1/reports/invoiced-receivables/export"},
		{"create-aging-export", http.MethodPost, "/api/v1/reports/aging/export"},
		{"export-status", http.MethodGet, "/api/v1/reports/exports/export-1"},
		{"export-download", http.MethodGet, "/api/v1/reports/exports/export-1/download"},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			grant := reportScopeGrant("CONTRACT")
			// 只准备会话步骤：若入口在 403 之前触达任何报表/任务SQL，脚本驱动会失败。
			db := scriptdb.Open(t, sessionStepsFor(t, reportSessionKey(), grant)...)
			handler := revisionHoldAuthHandler(t, db, grant)
			request := revisionHoldRequest(endpoint.method, endpoint.path, "", "qa-session-cookie")
			request.Header.Set("Idempotency-Key", "report-scope-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("受限范围访问 %s 应为 403，实际=%d body=%s", endpoint.path, response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "SETTLEMENT_FORBIDDEN") {
				t.Fatalf("受限范围访问 %s 未返回既有 forbidden 契约: %s", endpoint.path, response.Body.String())
			}
		})
	}
}

// TestReportDashboardAllowsApplicationDataScope 验证 APPLICATION 范围主体仍可读取看板，租户边界取
// 自可信会话而不是客户端声明。
// 参数：t 为测试上下文。
// 返回值：无；非 200 或租户绑定不符时失败。
func TestReportDashboardAllowsApplicationDataScope(t *testing.T) {
	grant := reportScopeGrant("APPLICATION")
	var args []driver.NamedValue
	steps := append(sessionStepsFor(t, reportSessionKey(), grant),
		scriptdb.Step{Kind: "query", SQL: "CASE WHEN due_date<? AND open_amount>0 THEN 1", Args: &args, Rows: [][]driver.Value{{int64(0), int64(0)}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan WHERE tenant_id=?", Rows: [][]driver.Value{{int64(0)}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_request WHERE tenant_id=?", Rows: [][]driver.Value{{int64(0)}}},
		scriptdb.Step{Kind: "query", SQL: "COALESCE(SUM(open_amount),0) FROM settlement_receivable WHERE tenant_id=?", Rows: [][]driver.Value{}},
		scriptdb.Step{Kind: "query", SQL: "COALESCE(SUM(amount),0) FROM settlement_receipt", Rows: [][]driver.Value{}},
		scriptdb.Step{Kind: "query", SQL: "ir.amount_incl_tax", Rows: [][]driver.Value{}},
		scriptdb.Step{Kind: "query", SQL: "AND due_date<? GROUP BY currency", Rows: [][]driver.Value{}},
	)
	handler := revisionHoldAuthHandler(t, scriptdb.Open(t, steps...), grant)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/dashboard?tenant_id=tenant-foreign", "", "qa-session-cookie"))
	if response.Code != http.StatusOK {
		t.Fatalf("APPLICATION 范围看板状态=%d body=%s", response.Code, response.Body.String())
	}
	if len(args) != 2 || args[1].Value != grant.TenantID {
		t.Fatalf("看板租户未绑定可信会话: %v", args)
	}
	if !strings.Contains(response.Body.String(), `"receivable_count":0`) {
		t.Fatalf("看板响应不符合契约: %s", response.Body.String())
	}
}

// TestReportExportCreationSnapshotsRequesterScope 验证创建导出任务时把申请人的授权范围快照写入任
// 务行，且快照不含租户、身份证明细和IP。
// 参数：t 为测试上下文。
// 返回值：无；未写入快照、快照字段不符或响应非 202 时失败。
func TestReportExportCreationSnapshotsRequesterScope(t *testing.T) {
	grant := reportScopeGrant("APPLICATION")
	var insertArgs []driver.NamedValue
	steps := append(sessionStepsFor(t, reportSessionKey(), grant),
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_report_export_job WHERE tenant_id=? AND requested_by=? AND idempotency_key=?", Err: sql.ErrNoRows},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_report_export_job", Args: &insertArgs},
		scriptdb.Step{Kind: "query", SQL: "SELECT report_type,status,file_name,error_message", Rows: [][]driver.Value{{"AGING_RECEIVABLES_CSV", "PENDING", "", "", time.Now().UTC(), nil, nil}}},
	)
	handler := revisionHoldAuthHandler(t, scriptdb.Open(t, steps...), grant)
	request := revisionHoldRequest(http.MethodPost, "/api/v1/reports/aging/export", "", "qa-session-cookie")
	request.Header.Set("Idempotency-Key", "report-scope-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("导出创建状态=%d body=%s", response.Code, response.Body.String())
	}
	if len(insertArgs) != 6 || insertArgs[1].Value != grant.TenantID || insertArgs[2].Value != grant.UserID {
		t.Fatalf("导出任务租户/申请人不符合契约: %v", insertArgs)
	}
	raw, ok := insertArgs[3].Value.([]byte)
	if !ok {
		t.Fatalf("requester_scope 绑定值类型=%T", insertArgs[3].Value)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("requester_scope 不是合法JSON: %v", err)
	}
	if snapshot["user_id"] != grant.UserID || snapshot["catalog_version"] != "4" {
		t.Fatalf("快照缺少申请人或目录版本: %s", raw)
	}
	permissions, _ := snapshot["permissions"].([]any)
	if len(permissions) != 2 {
		t.Fatalf("快照权限不符合契约: %s", raw)
	}
	for _, forbidden := range []string{"tenant_id", "identity_id", "login_ip", "user_login_ip"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("快照泄漏 %s: %s", forbidden, raw)
		}
	}
}

// reportSessionKey 返回与 revisionHoldAuthHandler 一致的会话密文密钥。
// 参数：无。
// 返回值：32字节 AES 密钥。
func reportSessionKey() []byte {
	return bytes.Repeat([]byte{7}, 32)
}
