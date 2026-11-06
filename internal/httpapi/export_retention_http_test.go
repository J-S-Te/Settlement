package httpapi

import (
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestExpiredReportExportDownloadStillGone 验证过期导出即使字节已被清理，下载仍返回 410，
// 而不是因内容为空误报“尚未生成完成”。
// 参数：t 为测试上下文。
// 返回值：无；状态非 410 或错误码不符时失败。
func TestExpiredReportExportDownloadStillGone(t *testing.T) {
	grant := reportScopeGrant("APPLICATION")
	steps := append(sessionStepsFor(t, reportSessionKey(), grant),
		scriptdb.Step{Kind: "query", SQL: "SELECT status,file_name,content_type,file_content,expires_at FROM settlement_report_export_job", Rows: [][]driver.Value{{"READY", "export.csv", "text/csv; charset=utf-8", nil, time.Now().UTC().Add(-time.Hour)}}},
	)
	handler := revisionHoldAuthHandler(t, scriptdb.Open(t, steps...), grant)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/reports/exports/export-1/download", "", "qa-session-cookie"))
	if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), "SETTLEMENT_EXPORT_EXPIRED") {
		t.Fatalf("过期导出下载状态=%d body=%s", response.Code, response.Body.String())
	}
}

// TestExpiredImportExportDownloadConflict 验证对账导出超过保留期后返回 EXPIRED 状态，
// 下载按既有“非 SUCCEEDED → 409”契约拒绝，不返回空成功文件。
// 参数：t 为测试上下文。
// 返回值：无；状态非 409 或错误码不符时失败。
func TestExpiredImportExportDownloadConflict(t *testing.T) {
	grant := revisionHoldPrincipal{
		TenantID:    "tenant-import",
		UserID:      "qa-finance",
		Roles:       []string{"settlement_finance_manager"},
		Permissions: []string{"settlement.import.reconcile"},
		ScopeType:   "APPLICATION",
	}
	steps := append(sessionStepsFor(t, reportSessionKey(), grant),
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_import_export SET content=NULL,status='EXPIRED',error_code='EXPORT_EXPIRED'"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_import_export WHERE tenant_id=?", Rows: [][]driver.Value{{"export-1", "EXPIRED", int64(1), int64(2), "0.00", "0.00", "EXPORT_EXPIRED", nil, time.Now().UTC()}}},
	)
	handler := revisionHoldAuthHandler(t, scriptdb.Open(t, steps...), grant)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, revisionHoldRequest(http.MethodGet, "/api/v1/settlement/contract-imports/snapshot-1/exports/export-1/download", "", "qa-session-cookie"))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "SETTLEMENT_CONFLICT") {
		t.Fatalf("过期对账导出下载状态=%d body=%s", response.Code, response.Body.String())
	}
}
