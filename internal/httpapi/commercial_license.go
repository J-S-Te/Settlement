package httpapi

import (
	"context"
	core "github.com/J-S-Te/license-core"
	"net/http"
	"strings"
)

type CommercialLicenseGate interface {
	Check(context.Context, core.Operation) error
}

func (a *API) checkInvoiceArchiveLicense(ctx context.Context) error {
	if a.commercial == nil {
		return core.ErrDenied
	}
	return a.commercial.Check(ctx, core.MUTATE_BUSINESS)
}

func commercialOperation(method, path string) core.Operation {
	if method == http.MethodOptions {
		return core.ESSENTIAL_SERVICE
	}
	for _, p := range []string{"/healthz", "/readyz", "/auth/login", "/auth/callback", "/auth/logout", "/auth/local-logout", "/auth/backchannel-logout", "/auth/me", "/api/v1/auth/me", "/logged-out"} {
		if path == p {
			return core.ESSENTIAL_SERVICE
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "notifications" && parts[4] == "read" && method == http.MethodPost {
		return core.ESSENTIAL_SERVICE
	}
	if method == http.MethodPost && (path == "/api/v1/reports/invoiced-receivables/export" || path == "/api/v1/reports/aging/export") {
		return core.EXPORT_HISTORY
	}
	if method != http.MethodGet {
		return core.MUTATE_BUSINESS
	}
	if path == "/internal/v1/handover/responsibilities" {
		return core.READ_HISTORY
	}
	if strings.HasPrefix(path, "/internal/v1/handover/transfers/") {
		receipt := strings.TrimPrefix(path, "/internal/v1/handover/transfers/")
		if receipt != "" && !strings.Contains(receipt, "/") {
			return core.READ_HISTORY
		}
	}
	for _, p := range []string{"/api/v1/dashboard", "/api/v1/reports/invoiced-receivables-top10", "/api/v1/receivable-plans", "/api/v1/receivables", "/api/v1/invoice-eligible-receivables", "/api/v1/receipts", "/api/v1/receipt-allocations", "/api/v1/invoice-requests", "/api/v1/tax-invoices", "/api/v1/dunning/policies", "/api/v1/dunning/cases", "/api/v1/dunning/overview", "/api/v1/dunning/coverage", "/api/v1/dunning/actions", "/api/v1/dunning/recipients", "/api/v1/notifications", "/api/v1/notifications/unread-count"} {
		if path == p {
			return core.READ_HISTORY
		}
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "receipts" && parts[4] == "matches" {
		return core.READ_HISTORY
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "tax-invoices" {
		return core.READ_HISTORY
	}
	if len(parts) == 7 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "tax-invoices" && parts[3] != "" && parts[4] == "documents" && parts[5] != "" && parts[6] == "download" {
		return core.EXPORT_HISTORY
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "outbox" && parts[3] == "events" {
		return core.READ_HISTORY
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "reports" && parts[3] == "aging" {
		return core.READ_HISTORY
	}
	if len(parts) >= 5 && len(parts) <= 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "reports" && parts[3] == "exports" {
		if len(parts) == 5 {
			return core.READ_HISTORY
		}
		if parts[5] == "download" {
			return core.EXPORT_HISTORY
		}
	}
	return core.MUTATE_BUSINESS
}
