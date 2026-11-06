package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
)

// TestContractEventReturnsCapabilityDisabledWhenIntegrationOff 验证合同联动
// 未启用时入口返回 404 能力未启用（C04：可选未启用与认证失败分开），
// 而不是伪装成令牌无效诱导发送方重试。
// 参数：t 为当前测试上下文。
// 返回值：无；禁用语义缺失或仍返回 401 时失败。
func TestContractEventReturnsCapabilityDisabledWhenIntegrationOff(t *testing.T) {
	handler := newMachineIngestHandler(config.Config{
		IntegrationEnabled: false, PublicOrigin: "http://localhost:5173",
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer anything")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "SETTLEMENT_CONTRACT_INTEGRATION_DISABLED") {
		t.Fatalf("status=%d body=%s, want 404 SETTLEMENT_CONTRACT_INTEGRATION_DISABLED", response.Code, response.Body.String())
	}
}

// TestContractEventDisabledTakesPrecedenceOverTokenCheck 验证禁用语义先于
// 令牌校验：集成关闭时即使令牌为空/伪造也返回 404 能力未启用。
// 参数：t 为当前测试上下文。
// 返回值：无；空令牌仍触发 401 时失败。
func TestContractEventDisabledTakesPrecedenceOverTokenCheck(t *testing.T) {
	handler := newMachineIngestHandler(config.Config{
		IntegrationEnabled: false, PublicOrigin: "http://localhost:5173",
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "SETTLEMENT_CONTRACT_INTEGRATION_DISABLED") {
		t.Fatalf("status=%d body=%s, want 404 SETTLEMENT_CONTRACT_INTEGRATION_DISABLED", response.Code, response.Body.String())
	}
}
