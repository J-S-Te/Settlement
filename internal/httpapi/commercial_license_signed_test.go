package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/J-S-Te/license-core"
	licruntime "github.com/J-S-Te/license-core/runtime"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/dunning"
	"github.com/j-s-te/settlement/internal/outbox"
	"github.com/j-s-te/settlement/internal/taxreconcile"
)

type signedSettlementGate struct {
	runtime *licruntime.FileRuntime
	now     time.Time
}

func (g *signedSettlementGate) Check(ctx context.Context, op core.Operation) error {
	return g.runtime.Evaluate(ctx, op, g.now)
}

func newSignedSettlementGate(t *testing.T, state licruntime.EnforcementState) (*signedSettlementGate, func()) {
	t.Helper()
	pp, ps, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vp, vs, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	binding := licruntime.Binding{InstanceID: "instance", Environment: "prod", Application: "settlement", ServiceID: "settlement-api"}
	path := filepath.Join(t.TempDir(), "state.json")
	platformKeys := map[string]ed25519.PublicKey{"platform": pp}
	vendorKeys := map[string]ed25519.PublicKey{"vendor": vp}
	l := core.License{ProtocolVersion: core.ProtocolVersion, Issuer: core.Issuer, ID: "test-license", Version: 1, CustomerID: "customer", ProductID: core.Product, Environment: binding.Environment, InstanceID: binding.InstanceID, IssuedAt: now.Unix() - 100, NotBefore: now.Unix() - 100, Applications: []core.Application{{Code: "settlement", Kind: "FULL", NotBefore: now.Unix() - 100, ExpiresAt: now.Unix() + 10}}}
	raw, err := core.Sign(l, "vendor", vs)
	if err != nil {
		t.Fatal(err)
	}
	s := licruntime.PlatformSnapshot{Protocol: licruntime.Protocol, InstanceID: binding.InstanceID, Environment: binding.Environment, Application: binding.Application, ServiceID: binding.ServiceID, Revision: 1, EnforcementState: state, MigrationEligible: state == licruntime.Pending, CurrentLicenseJWS: raw, HighestLicenseVersion: 1, SnapshotIssuedAt: now.Unix()}
	snapshot, err := licruntime.SignSnapshot(s, "platform", ps)
	if err != nil {
		t.Fatal(err)
	}
	r, err := licruntime.NewFileRuntime(path, binding, platformKeys, vendorKeys)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.ApplySnapshot(context.Background(), snapshot, now); err != nil {
		t.Fatal(err)
	}
	g := &signedSettlementGate{runtime: r, now: now.Add(9 * time.Second)}
	restart := func() {
		t.Helper()
		g.runtime, err = licruntime.NewFileRuntime(path, binding, platformKeys, vendorKeys)
		if err != nil {
			t.Fatal(err)
		}
	}
	return g, restart
}

func TestSignedExpiryDeniesSettlementHTTPBeforeIdentityAndBusiness(t *testing.T) {
	mutations := []string{
		"/api/v1/receipts", "/api/v1/invoice-requests", "/api/v1/invoice-requests/id/approve",
		"/api/v1/invoice-requests/id/recover-failure", "/api/v1/invoice-requests/id/manual-issue",
		"/api/v1/receivable-plans/id/confirm", "/api/v1/receipt-allocations", "/api/v1/tax-invoices/id/red-flush",
		"/api/v1/dunning/policies", "/api/v1/credit-sync-events/id/retry",
		"/internal/v1/settlement/events/contracts", "/internal/v1/settlement/events/tax-results",
	}
	for _, state := range []licruntime.EnforcementState{licruntime.Pending, licruntime.Applying, licruntime.Enforced} {
		t.Run(string(state), func(t *testing.T) {
			gate, restart := newSignedSettlementGate(t, state)
			// 有意不提供生产身份适配器：有效许可证
			// 会到达既有边界，过期则在角色查询前停止。
			h := New(nil, config.Config{PublicOrigin: "https://platform.test"}, slog.Default(), nil, nil, nil, gate)
			request := func(path string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
				r.Header.Set("Origin", "https://platform.test")
				r.Header.Set("X-CSRF-Token", "1")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			if w := request("/api/v1/receipts"); w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "COMMERCIAL_LICENSE_DENIED") {
				t.Fatalf("unexpired request did not reach identity boundary: %d %s", w.Code, w.Body.String())
			}
			for _, offset := range []int{10, 11} {
				gate.now = time.Unix(1800000000+int64(offset), 0)
				for _, path := range mutations {
					if w := request(path); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "COMMERCIAL_LICENSE_DENIED") {
						t.Fatalf("expired mutation %s: %d %s", path, w.Code, w.Body.String())
					}
				}
			}
			// 没有新快照到达。持久高水位使过期约束在
			// 重启和小幅时钟回拨后仍有效。
			restart()
			gate.now = time.Unix(1800000005, 0)
			if w := request("/api/v1/invoice-requests/id/approve"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "COMMERCIAL_LICENSE_DENIED") {
				t.Fatalf("restart resurrected expired approval: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSignedExpiredSettlementHistoryRetainsIdentityBoundary(t *testing.T) {
	gate, _ := newSignedSettlementGate(t, licruntime.Pending)
	gate.now = time.Unix(1800000010, 0)
	h := New(nil, config.Config{PublicOrigin: "https://platform.test"}, slog.Default(), nil, nil, nil, gate)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/receivables"},
		{http.MethodPost, "/api/v1/reports/aging/export"},
		{http.MethodPost, "/api/v1/reports/invoiced-receivables/export"},
		{http.MethodGet, "/api/v1/reports/exports/id/download"},
		{http.MethodGet, "/api/v1/tax-invoices/invoice/documents/document/download"},
		{http.MethodPost, "/api/v1/notifications/id/read"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Origin", "https://platform.test")
		r.Header.Set("X-CSRF-Token", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "SETTLEMENT_IDENTITY_UNAVAILABLE") {
			t.Fatalf("historical/essential path bypassed identity or denied by license %s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/tax-invoices/invoice/download", "/api/v1/new-computation"} {
		if err := gate.Check(context.Background(), commercialOperation(http.MethodGet, path)); !errors.Is(err, core.ErrDenied) {
			t.Fatalf("unknown historical alias allowed: %s: %v", path, err)
		}
	}
}

func TestSignedExpiredSettlementTasksStopBeforeClaimTokenAndDatabase(t *testing.T) {
	gate, _ := newSignedSettlementGate(t, licruntime.Pending)
	gate.now = time.Unix(1800000010, 0)
	check := func(ctx context.Context) error { return gate.Check(ctx, core.MUTATE_BUSINESS) }
	ctx := context.Background()
	scanner := &dunning.Scanner{CheckBusinessLicense: check}
	if err := scanner.RunOnce(ctx); !errors.Is(err, core.ErrDenied) {
		t.Fatalf("dunning acquired business: %v", err)
	}
	reconcile := &taxreconcile.Worker{CheckBusinessLicense: check}
	if err := reconcile.RunOnce(ctx); !errors.Is(err, core.ErrDenied) {
		t.Fatalf("tax reconciliation acquired business: %v", err)
	}
	worker := &outbox.Worker{CheckBusinessLicense: check, Destinations: []outbox.Destination{{Name: "TAX_INVOICE_COMMAND"}}}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("paused tax outbox must not acquire or consume retries: %v", err)
	}
}
