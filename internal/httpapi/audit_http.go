package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/j-s-te/settlement/internal/service"
)

// auditStatusWriter 捕获写请求最终状态码；通用审计只读取状态，不改变响应内容。
// 仅用于 /api/v1 用户写路由，响应均为 JSON，不涉及文件流式下载。
type auditStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditStatusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func (w *auditStatusWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// genericAuditEnabled 只在审计目的地已配置或必开时启用通用 HTTP 审计；
// 未配置审计的本地/精简部署不应产生无法投递的 outbox 噪音。
func (a *API) genericAuditEnabled() bool {
	return a != nil && (a.cfg.AuditDestinationRequired || a.cfg.AuditDestinationConfigured)
}

// shouldAuditHTTP 只覆盖用户会话的 /api/v1 写操作；认证、健康检查、内部机器接口
// 有各自的审计/安全边界，不能混入租户操作流水。
func (a *API) shouldAuditHTTP(r *http.Request) bool {
	if a == nil || r == nil || r.URL == nil {
		return false
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	path := r.URL.Path
	return strings.HasPrefix(path, "/api/v1/") && !strings.HasPrefix(path, "/api/v1/auth/")
}

// auditHTTP 在业务响应完成后把通用写操作补进本地审计与平台 outbox。
// 这是兜底事件：高风险业务动作仍保留更完整的业务审计事件，二者用 action 前缀区分。
// 审计写入失败只记 error，不回滚已经提交的业务事实；outbox 负责后续重试。
func (a *API) auditHTTP(r *http.Request, p service.Principal, requestID string, status int) {
	if a == nil || a.service == nil || a.service.DB == nil || r == nil || r.URL == nil {
		return
	}
	result := "SUCCESS"
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		result = "DENIED"
	case status >= http.StatusBadRequest:
		result = "FAILURE"
	}
	risk := "LOW"
	if r.Method == http.MethodDelete || status >= http.StatusInternalServerError || containsAuditRiskWord(r.URL.Path) {
		risk = "HIGH"
	} else if status >= http.StatusBadRequest {
		risk = "MEDIUM"
	}
	action := auditActionFor(r.Method, r.URL.Path)
	resourceID := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	if resourceID == "" {
		resourceID = requestID
	}
	detail := map[string]any{
		"method": r.Method, "path": r.URL.Path, "http_status": status,
		"result": result, "risk_level": risk,
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer cancel()
	if err := a.service.AuditAction(ctx, p, action, "settlement_http", resourceID, detail); err != nil {
		// 通用审计失败必须可告警；不能把已经提交的业务响应改成失败。
		slog.ErrorContext(r.Context(), "settlement generic HTTP audit failed",
			"route", r.URL.Path, "method", r.Method, "status", status, "request_id", requestID, "error", err)
	}
}

// auditActionFor 只保留稳定的静态路由段，避免把单据 id 写进 action 造成高基数；
// 精确目标保留在 resource_id 与 detail.path 中。
func auditActionFor(method, path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	static := make([]string, 0, 3)
	for _, part := range parts {
		if part == "" || part == "api" || part == "v1" || auditPathSegmentIsID(part) {
			continue
		}
		static = append(static, part)
		if len(static) == 2 {
			break
		}
	}
	if len(static) == 0 {
		return "SETTLEMENT_HTTP_" + method
	}
	return "SETTLEMENT_HTTP_" + method + "_" + strings.ToUpper(strings.Join(static, "."))
}

func auditPathSegmentIsID(value string) bool {
	if value == "" {
		return false
	}
	if len(value) == 26 || len(value) == 36 {
		return true
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' && r != '_' {
			return false
		}
	}
	allDigits := true
	for _, r := range value {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	return allDigits
}

func containsAuditRiskWord(path string) bool {
	lowered := strings.ToLower(path)
	for _, word := range []string{"invoice", "receipt", "allocation", "reversal", "approval", "approve", "reject", "export", "download", "outbox"} {
		if strings.Contains(lowered, word) {
			return true
		}
	}
	return false
}
