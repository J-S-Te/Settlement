package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
)

// TestImportExportRouteRejectsInvalidIdempotencyKey 验证非法重放键在进入业务事务前返回400。
// 参数：t为测试上下文。
// 返回值：无，超长或含控制字符的键被受理时失败。
func TestImportExportRouteRejectsInvalidIdempotencyKey(t *testing.T) {
	for name, key := range map[string]string{
		"too-long": strings.Repeat("k", 129),
		"nul":      "export\x00key",
		"tab":      "export\tkey",
	} {
		t.Run(name, func(t *testing.T) {
			handler := New(nil, config.Config{DevelopmentAuth: true, PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/settlement/contract-imports/snapshot-1/exports", strings.NewReader(`{}`))
			r.Header.Set("Origin", "https://settlement.test")
			r.Header.Set("X-CSRF-Token", "1")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", key)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "SETTLEMENT_INVALID_REQUEST") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestImportExportIdempotencyKeyBoundary 验证重放键去空格、128字节边界与控制字符判定。
// 参数：t为测试上下文。
// 返回值：无，边界值判定不符时失败。
func TestImportExportIdempotencyKeyBoundary(t *testing.T) {
	if key, ok := importExportIdempotencyKey("  " + strings.Repeat("k", 128) + "  "); !ok || key != strings.Repeat("k", 128) {
		t.Fatalf("128-byte key rejected: ok=%v len=%d", ok, len(key))
	}
	if _, ok := importExportIdempotencyKey(strings.Repeat("k", 129)); ok {
		t.Fatal("129-byte key must be rejected")
	}
	if key, ok := importExportIdempotencyKey("   "); !ok || key != "" {
		t.Fatalf("blank key must degrade to empty, got %q ok=%v", key, ok)
	}
	if _, ok := importExportIdempotencyKey("export\x7fkey"); ok {
		t.Fatal("control character key must be rejected")
	}
}
