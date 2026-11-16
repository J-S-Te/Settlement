package httpapi

import (
	"database/sql/driver"
	"errors"
	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSettlementMetricStreamCommitAndTenantBoundary 验证真实HTTP租户绑定及源提交失败不输出end。
// 参数：t 为隔离脚本数据库和HTTP测试上下文。
// 返回值：无；提交失败输出完整end或使用伪造租户时失败。
func TestSettlementMetricStreamCommitAndTenantBoundary(t *testing.T) {
	path := "/internal/v1/dashboard/metrics/stream"
	if commercialOperation("GET", path) != core.READ_HISTORY {
		t.Fatal("license classification")
	}
	for _, failCommit := range []bool{false, true} {
		var arguments []driver.NamedValue
		var commitErr error
		if failCommit {
			commitErr = errors.New("commit failed")
		}
		db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "FROM settlement_recognition", Columns: 1, Args: &arguments}, scriptdb.Step{Kind: "commit", Err: commitErr})
		api := &API{cfg: config.Config{DashboardMachineEnabled: true, OIDCTenantID: "signed-tenant"}, dashboardMachine: dashboardVerifierStub{identity: platform.ServiceIdentity{TenantID: "signed-tenant"}}, service: &service.Service{DB: db}}
		req := httptest.NewRequest("GET", path+"?tenant_id=forged", nil)
		req.Header.Set("Authorization", "Bearer synthetic")
		req.Header.Set("X-Tenant-ID", "forged")
		out := httptest.NewRecorder()
		api.ServeHTTP(out, req)
		if out.Code != 200 || strings.Contains(out.Body.String(), `"type":"end"`) == failCommit {
			t.Fatalf("commit=%v status=%d body=%s", failCommit, out.Code, out.Body.String())
		}
		if len(arguments) != 2 || arguments[0].Value != "signed-tenant" {
			t.Fatalf("untrusted tenant: %v", arguments)
		}
	}
}
