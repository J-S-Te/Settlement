package httpapi

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
)

// SEC-D10：DevelopmentAuth 启用时必须在启动期输出响亮警告（可告警的 error 日志）。
func TestDevelopmentAuthStartupLogsLoudWarning(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	New(nil, config.Config{DevelopmentAuth: true, PublicOrigin: "http://localhost:5173", OIDCEnvironmentCode: "dev"}, logger, nil, nil, nil)

	output := logs.String()
	if !strings.Contains(output, "DEVELOPMENT AUTH ENABLED") {
		t.Fatalf("missing loud DevelopmentAuth warning: %s", output)
	}
	if !strings.Contains(output, "development_auth") || !strings.Contains(output, "environment_code") {
		t.Fatalf("warning must carry alertable fields: %s", output)
	}
}

// SEC-D10：DevelopmentAuth 关闭时不得输出该警告（避免告警疲劳）。
func TestNoDevelopmentAuthWarningWhenDisabled(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	New(nil, config.Config{DevelopmentAuth: false, PublicOrigin: "https://platform.example.com", OIDCEnvironmentCode: "prod"}, logger, nil, nil, nil)

	if strings.Contains(logs.String(), "DEVELOPMENT AUTH ENABLED") {
		t.Fatalf("unexpected warning: %s", logs.String())
	}
}
