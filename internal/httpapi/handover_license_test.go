package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	licruntime "github.com/J-S-Te/license-core/runtime"
	"github.com/j-s-te/settlement/internal/platform"
)

func TestSignedExpiredHandoverHistoryRetainsMachineBoundary(t *testing.T) {
	gate, _ := newSignedSettlementGate(t, licruntime.Pending)
	gate.now = time.Unix(1800000010, 0)
	api := handoverAPI(t, &handoverRepositoryStub{receiptFound: true}, handoverVerifierStub{identity: platform.ServiceIdentity{TenantID: "tenant-1"}})
	api.commercial = gate
	for _, path := range []string{"/internal/v1/handover/responsibilities?user_id=collector", "/internal/v1/handover/transfers/receipt-1?idempotency_key=key-1"} {
		for _, bearer := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			if bearer {
				r.Header.Set("Authorization", "Bearer machine-token")
			}
			w := httptest.NewRecorder()
			api.ServeHTTP(w, r)
			want := http.StatusUnauthorized
			if bearer {
				want = http.StatusOK
			}
			if w.Code != want || strings.Contains(w.Body.String(), "COMMERCIAL_LICENSE_DENIED") {
				t.Fatalf("history %s bearer=%v: %d %s", path, bearer, w.Code, w.Body.String())
			}
		}
	}
	for _, path := range []string{"/internal/v1/handover/freeze", "/internal/v1/handover/transfers", "/internal/v1/handover/complete"} {
		w := httptest.NewRecorder()
		api.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "COMMERCIAL_LICENSE_DENIED") {
			t.Fatalf("handover mutation after expiry: %d %s", w.Code, w.Body.String())
		}
	}
}
