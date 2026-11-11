package httpapi

import (
	"database/sql/driver"
	"encoding/json"
	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
	"net/http/httptest"
	"testing"
)

// TestRecognitionRoutesFailClosed 验证新财务入口具有身份边界且机器未配置失败关闭。
// 参数：t为测试上下文。
// 返回值：无，入口缺失或安全边界错误时失败。
func TestRecognitionRoutesFailClosed(t *testing.T) {
	api := New(nil, config.Config{}, nil, nil, nil, nil)
	for _, test := range []struct {
		path   string
		status int
	}{
		{"/api/v1/recognitions", 503}, {"/internal/v1/dashboard/metrics", 503},
	} {
		out := httptest.NewRecorder()
		api.ServeHTTP(out, httptest.NewRequest("GET", test.path, nil))
		if out.Code != test.status {
			t.Errorf("%s: got %d want %d", test.path, out.Code, test.status)
		}
	}
}

// TestRecognitionMetricsEnvelopeAndLicense 验证真实包络、签名租户与只读许可分类。
// 参数：t为HTTP契约上下文，数据库边界使用显式脚本驱动。
// 返回值：无，tenant参数扩权或缺Code包络时失败。
func TestRecognitionMetricsEnvelopeAndLicense(t *testing.T) {
	for _, test := range []struct {
		method, path string
		want         core.Operation
	}{{"GET", "/internal/v1/dashboard/metrics", core.READ_HISTORY}, {"GET", "/api/v1/recognitions", core.READ_HISTORY}, {"POST", "/api/v1/recognitions", core.MUTATE_BUSINESS}, {"GET", "/api/v1/recognitions/evidence/id/download", core.EXPORT_HISTORY}} {
		if commercialOperation(test.method, test.path) != test.want {
			t.Fatalf("license mismatch %s", test.path)
		}
	}
	var arguments []driver.NamedValue
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "FROM settlement_recognition", Columns: 1, Args: &arguments}, scriptdb.Step{Kind: "commit"})
	api := &API{cfg: config.Config{DashboardMachineEnabled: true, OIDCTenantID: "signed-tenant"}, dashboardMachine: dashboardVerifierStub{identity: platform.ServiceIdentity{TenantID: "signed-tenant"}}, service: &service.Service{DB: db}}
	request := httptest.NewRequest("GET", "/internal/v1/dashboard/metrics?tenant_id=other", nil)
	request.Header.Set("Authorization", "Bearer synthetic")
	request.Header.Set("X-Tenant-ID", "other")
	out := httptest.NewRecorder()
	api.ServeHTTP(out, request)
	var envelope struct {
		Code string                     `json:"code"`
		Data service.RecognitionMetrics `json:"data"`
	}
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &envelope) != nil || envelope.Code != "OK" || envelope.Data.TenantID != "signed-tenant" || !envelope.Data.Complete || envelope.Data.Rows == nil {
		t.Fatalf("HTTP=%d body=%s", out.Code, out.Body.String())
	}
	if len(arguments) != 1 || arguments[0].Value != "signed-tenant" {
		t.Fatalf("untrusted tenant used: %v", arguments)
	}
}
