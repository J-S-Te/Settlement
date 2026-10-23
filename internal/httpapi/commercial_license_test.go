package httpapi

import (
	core "github.com/J-S-Te/license-core"
	"testing"
)

func TestLicenseOperationsKeepHistoryExportAndDenyNewBusiness(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		op           core.Operation
	}{{"POST", "/api/v1/reports/aging/export", core.EXPORT_HISTORY}, {"GET", "/api/v1/reports/exports/id/download", core.EXPORT_HISTORY}, {"GET", "/api/v1/receivables", core.READ_HISTORY}, {"GET", "/api/v1/new-computation", core.MUTATE_BUSINESS}, {"POST", "/internal/v1/settlement/events/tax-results", core.MUTATE_BUSINESS}, {"POST", "/api/v1/notifications/id/read", core.ESSENTIAL_SERVICE}} {
		if got := commercialOperation(tc.method, tc.path); got != tc.op {
			t.Fatal(tc, got)
		}
	}
}

func TestHandoverHistoryClassificationIsExact(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         core.Operation
	}{
		{"GET", "/internal/v1/handover/responsibilities", core.READ_HISTORY},
		{"GET", "/internal/v1/handover/transfers/receipt-1", core.READ_HISTORY},
		{"POST", "/internal/v1/handover/freeze", core.MUTATE_BUSINESS},
		{"POST", "/internal/v1/handover/transfers", core.MUTATE_BUSINESS},
		{"POST", "/internal/v1/handover/complete", core.MUTATE_BUSINESS},
		{"GET", "/internal/v1/handover/transfers", core.MUTATE_BUSINESS},
		{"GET", "/internal/v1/handover/transfers/", core.MUTATE_BUSINESS},
		{"GET", "/internal/v1/handover/transfers/id/extra", core.MUTATE_BUSINESS},
		{"GET", "/internal/v1/handover/responsibilities/extra", core.MUTATE_BUSINESS},
		{"POST", "/internal/v1/handover/responsibilities", core.MUTATE_BUSINESS},
	} {
		if got := commercialOperation(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s = %s, want %s", tc.method, tc.path, got, tc.want)
		}
	}
}
