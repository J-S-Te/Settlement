package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/platform"
)

func TestInvoiceRecoveryRequiresAuthentication(t *testing.T) {
	handler := New(nil, config.Config{PublicOrigin: "http://localhost:8081"}, slog.Default(), &platform.Authenticator{}, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-requests/request/recover-failure", strings.NewReader(`{"version":1,"reason":"已核实税控失败"}`))
	r.Header.Set("Origin", "http://localhost:8081")
	r.Header.Set("X-CSRF-Token", "1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestInvoiceRecoveryRejectsCSRF(t *testing.T) {
	handler := New(nil, config.Config{PublicOrigin: "http://localhost:8081", DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-requests/request/recover-failure", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
