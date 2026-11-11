package httpapi

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	core "github.com/J-S-Te/license-core"
	licruntime "github.com/J-S-Te/license-core/runtime"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type dashboardVerifierStub struct {
	identity platform.ServiceIdentity
	err      error
}

// Verify 提供合成鉴权边界供HTTP契约测试使用。
// 参数：上下文和令牌为本地合成输入。
// 返回值：测试指定的可信身份或错误。
func (s dashboardVerifierStub) Verify(context.Context, string) (platform.ServiceIdentity, error) {
	return s.identity, s.err
}

// TestDashboardFinanceRouteRequiresDedicatedMachineAndHistoryLicense 验证独立机器入口。
// 参数：t为测试上下文。
// 返回值：无，匿名、未配置、跨租户或许可边界错误时失败。
func TestDashboardFinanceRouteRequiresDedicatedMachineAndHistoryLicense(t *testing.T) {
	if commercialOperation(http.MethodGet, "/internal/v1/dashboard/finance") != core.READ_HISTORY || commercialOperation(http.MethodPost, "/internal/v1/dashboard/finance") != core.MUTATE_BUSINESS {
		t.Fatal("wrong license classification")
	}
	for _, tc := range []struct {
		name     string
		enabled  bool
		verifier handoverVerifier
		token    string
		want     int
	}{
		{"disabled", false, nil, "", 503}, {"incomplete", true, nil, "", 503},
		{"anonymous", true, dashboardVerifierStub{}, "", 401},
		{"invalid", true, dashboardVerifierStub{err: errors.New("rejected")}, "synthetic", 401},
		{"wrong-tenant", true, dashboardVerifierStub{identity: platform.ServiceIdentity{TenantID: "other"}}, "synthetic", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &API{cfg: config.Config{DashboardMachineEnabled: tc.enabled, OIDCTenantID: "tenant"}, dashboardMachine: tc.verifier}
			r := httptest.NewRequest("GET", "/internal/v1/dashboard/finance?tenant_id=other", nil)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("HTTP=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable r", Columns: 6, Rows: [][]driver.Value{}}, scriptdb.Step{Kind: "query", SQL: "FROM settlement_receipt rc", Columns: 3}, scriptdb.Step{Kind: "commit"})
	a := &API{cfg: config.Config{DashboardMachineEnabled: true, OIDCTenantID: "tenant"}, dashboardMachine: dashboardVerifierStub{identity: platform.ServiceIdentity{TenantID: "tenant", ApplicationCode: "data_analysis", EnvironmentCode: "prod"}}, service: &service.Service{DB: db}}
	r := httptest.NewRequest("GET", "/internal/v1/dashboard/finance?tenant_id=other", nil)
	r.Header.Set("Authorization", "Bearer synthetic")
	r.Header.Set("X-Tenant-ID", "other")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	var got service.FinanceDashboard
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !got.Available || got.TenantID != "tenant" || got.Metrics.Currencies == nil {
		t.Fatalf("HTTP=%d body=%s", w.Code, w.Body.String())
	}
}

// TestFinanceDashboardExpiredLicensePreservesMachineBoundary 验证签名只读期边界。
// 参数：t为测试上下文。
// 返回值：无，历史许可错误放宽认证或阻断合法读取时失败。
func TestFinanceDashboardExpiredLicensePreservesMachineBoundary(t *testing.T) {
	gate, _ := newSignedSettlementGate(t, licruntime.Pending)
	gate.now = time.Unix(1800000010, 0)
	a := &API{cfg: config.Config{DashboardMachineEnabled: true, OIDCTenantID: "tenant"}, dashboardMachine: dashboardVerifierStub{identity: platform.ServiceIdentity{TenantID: "tenant"}}, commercial: gate}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/internal/v1/dashboard/finance", nil))
	if w.Code != 401 {
		t.Fatalf("anonymous after expiry=%d body=%s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/internal/v1/dashboard/finance/", "/internal/v1/dashboard/finance/other"} {
		if commercialOperation("GET", path) != core.MUTATE_BUSINESS {
			t.Fatalf("unreviewed path inherited history: %s", path)
		}
	}
}
