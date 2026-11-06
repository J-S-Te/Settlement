package httpapi

import (
	"bytes"
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestNativeHTTPUsesOnlinePermissionAndFeatureGate 验证真实OIDC入口的权限与默认关闭门禁。
// 参数：t 为测试上下文。
// 返回值：无；拒绝前触达业务数据库或默认开放本地功能时失败。
func TestNativeHTTPUsesOnlinePermissionAndFeatureGate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions []string
		want        int
		code        string
	}{
		{"missing permission", []string{"settlement.report.read"}, http.StatusForbidden, "SETTLEMENT_FORBIDDEN"},
		{"feature closed", []string{"settlement.receivable.read"}, http.StatusConflict, "SETTLEMENT_NATIVE_FINANCIAL_DISABLED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grant := revisionHoldPrincipal{TenantID: "tenant-a", UserID: "finance", Roles: []string{"settlement_finance_manager"}, Permissions: tc.permissions, ScopeType: "TENANT"}
			handler := revisionHoldAuthHandler(t, scriptdb.Open(t, sessionStepsFor(t, bytes.Repeat([]byte{7}, 32), grant)...), grant)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/financial-baselines", "", "session"))
			if response.Code != tc.want || !strings.Contains(response.Body.String(), tc.code) {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
		})
	}
}

// TestNativeHTTPReadOnlyCapabilitiesFailClosed 验证能力读取保持旧开关默认值且不新增授权。
// 参数：t 为测试上下文。
// 返回值：无；默认显示可创建或暴露数据库数据时失败。
func TestNativeHTTPReadOnlyCapabilitiesFailClosed(t *testing.T) {
	grant := revisionHoldPrincipal{TenantID: "tenant-a", UserID: "finance", Roles: []string{"settlement_finance_manager"}, Permissions: []string{"settlement.receivable.read"}, ScopeType: "TENANT"}
	handler := revisionHoldAuthHandler(t, scriptdb.Open(t, sessionStepsFor(t, bytes.Repeat([]byte{7}, 32), grant)...), grant)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/financial-baselines/capabilities", "", "session"))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"native_financial_enabled":false`) || !strings.Contains(response.Body.String(), `"can_create":false`) {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}

// TestNativeHTTPRetainsCSRFAndCommercialLicense 验证独立接口依然在原CSRF和商业许可之后。
// 参数：t 为测试上下文。
// 返回值：无；拒绝前触达身份或业务数据库时失败。
func TestNativeHTTPRetainsCSRFAndCommercialLicense(t *testing.T) {
	for _, kind := range []string{"csrf", "license"} {
		t.Run(kind, func(t *testing.T) {
			a := &API{service: &service.Service{NativeFinancialEnabled: true}}
			request := revisionHoldRequest(http.MethodPost, "/api/v1/financial-customers", `{"name":"local"}`, "")
			code := "SETTLEMENT_CSRF_REJECTED"
			if kind == "license" {
				a.commercial = &revisionHoldReadOnlyLicense{}
				code = "COMMERCIAL_LICENSE_DENIED"
			}
			response := httptest.NewRecorder()
			a.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), code) {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
		})
	}
}

// TestNativeHTTPListBindsTenantAndPreservesNull 验证本地列表走真实入口、可信租户和可空合同协议。
// 参数：t 为测试上下文。
// 返回值：无；客户端租户覆盖、NULL崩溃或本地来源标签缺失时失败。
func TestNativeHTTPListBindsTenantAndPreservesNull(t *testing.T) {
	grant := revisionHoldPrincipal{TenantID: "tenant-a", UserID: "finance", Roles: []string{"settlement_finance_manager"}, Permissions: []string{"settlement.receivable.read"}, ScopeType: "TENANT"}
	var args []driver.NamedValue
	rows := [][]driver.Value{{"id", service.NativeFinancialBasis, "id", "FB-1", int64(1), service.NativeCustomerPrefix + strings.Repeat("A", 32), "local", "CNY", "100.00", "2020-01-01", "EFFECTIVE", "PURCHASE_ORDER", "PO-1", "note", nil, "", "settlement_local"}}
	steps := append(sessionStepsFor(t, bytes.Repeat([]byte{7}, 32), grant), scriptdb.Step{Kind: "query", SQL: "AND basis_type='LOCAL_FINANCIAL'", Args: &args, Rows: rows})
	handler := revisionHoldAuthHandler(t, scriptdb.Open(t, steps...), grant)
	handler.(*API).service.ConfigureFinancialSources(true, false)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/financial-baselines?tenant_id=foreign&limit=20&offset=40", "", "session"))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"source_contract_id":null`) || !strings.Contains(response.Body.String(), `"basis_type":"LOCAL_FINANCIAL"`) {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	if len(args) != 3 || args[0].Value != "tenant-a" || args[1].Value != int64(20) || args[2].Value != int64(40) {
		t.Fatalf("args=%+v", args)
	}
}

// TestNativeFinancialReferencesPreserveBothSources 验证原人工计划候选同时显示真实合同和本地依据且不造责任。
// 参数：t 为测试上下文。
// 返回值：无；本地被过滤、冒充合同编号或可信租户丢失时失败。
func TestNativeFinancialReferencesPreserveBothSources(t *testing.T) {
	grant := revisionHoldPrincipal{TenantID: "tenant-a", UserID: "finance", Roles: []string{"settlement_finance_manager"}, Permissions: []string{"settlement.receivable.read"}, ScopeType: "TENANT"}
	var args []driver.NamedValue
	var statement string
	rows := [][]driver.Value{
		{"local-id", "", service.NativeCustomerPrefix + strings.Repeat("A", 32), "本地客户", "", "CNY", "100.00", "", "LOCAL_FINANCIAL", "local-id", "FB-1", int64(1), "settlement_local"},
		{"contract-id", "HT-1", "7", "原合同客户", "project", "CNY", "200.00", "sales-owner", "CONTRACT", "", "", int64(0), "legacy"},
	}
	steps := append(sessionStepsFor(t, bytes.Repeat([]byte{7}, 32), grant), scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_snapshot cs LEFT JOIN settlement_contract_responsibility", Args: &args, SQLOut: &statement, Rows: rows})
	handler := revisionHoldAuthHandler(t, scriptdb.Open(t, steps...), grant)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/contract-references?tenant_id=foreign&limit=17&offset=5", "", "session"))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"contract_no":""`) || !strings.Contains(body, `"local_document_no":"FB-1"`) || !strings.Contains(body, `"contract_no":"HT-1"`) || !strings.Contains(body, `"owner_user_id":""`) {
		t.Fatalf("%d %s", response.Code, body)
	}
	if len(args) != 3 || args[0].Value != "tenant-a" || args[1].Value != int64(17) || args[2].Value != int64(5) || !strings.Contains(statement, "ORDER BY cs.updated_at DESC,cs.id LIMIT ? OFFSET ?") || strings.Contains(statement, "basis_type='CONTRACT'") {
		t.Fatalf("sql=%s args=%+v", statement, args)
	}
}
