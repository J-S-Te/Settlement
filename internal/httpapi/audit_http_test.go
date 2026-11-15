package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/service"
)

func TestShouldAuditHTTPCoversUserWritesOnly(t *testing.T) {
	api := &API{}
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/api/v1/receipts", true},
		{http.MethodPut, "/api/v1/receivable-plans/plan-1", true},
		{http.MethodPatch, "/api/v1/invoices/inv-1", true},
		{http.MethodDelete, "/api/v1/receipts/r-1", true},
		{http.MethodPost, "/api/v1/auth/me", false},
		{http.MethodGet, "/api/v1/receipts", false},
		{http.MethodHead, "/api/v1/receipts", false},
		{http.MethodPost, "/internal/v1/settlement/events/contracts", false},
		{http.MethodPost, "/healthz", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if got := api.shouldAuditHTTP(r); got != tc.want {
			t.Fatalf("shouldAuditHTTP(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestAuditStatusWriterCapturesFinalStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &auditStatusWriter{ResponseWriter: recorder}
	writer.WriteHeader(http.StatusCreated)
	if _, err := writer.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if writer.Status() != http.StatusCreated {
		t.Fatalf("status = %d, want %d", writer.Status(), http.StatusCreated)
	}
	if recorder.Code != http.StatusCreated {
		t.Fatalf("underlying status = %d, want %d", recorder.Code, http.StatusCreated)
	}
}

func TestAuditHTTPWithoutServiceIsSafe(t *testing.T) {
	api := &API{}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/receipts", nil)
	api.auditHTTP(r, service.Principal{TenantID: "tenant-1", UserID: "user-1"}, "req-1", http.StatusBadGateway)
}

func TestAuditActionForSkipsDynamicIDs(t *testing.T) {
	cases := map[string]string{
		"POST /api/v1/receipts":                          "SETTLEMENT_HTTP_POST_RECEIPTS",
		"POST /api/v1/receipts/01ARZ3NDEKTSV4RRFFQ69G5FAV": "SETTLEMENT_HTTP_POST_RECEIPTS",
		"POST /api/v1/reports/aging/export":              "SETTLEMENT_HTTP_POST_REPORTS.AGING",
	}
	for input, want := range cases {
		parts := strings.SplitN(input, " ", 2)
		if got := auditActionFor(parts[0], parts[1]); got != want {
			t.Fatalf("auditActionFor(%q) = %q, want %q", input, got, want)
		}
	}
}
