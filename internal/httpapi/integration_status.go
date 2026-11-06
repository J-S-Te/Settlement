package httpapi

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/j-s-te/settlement/internal/outbox"
)

// BIZ-XAPI-10：集成目的地缺失/积压必须在 /readyz 与 /metrics 可见。
// 本文件只做纯判定与渲染，实际查询由 API.integrationSnapshot 提供，便于单测覆盖
// “required 缺失 ⇒ 503、非 required 缺失 ⇒ degraded、未配置目的地积压计数”。
type integrationSnapshot struct {
	AuditRequired          bool
	AuditConfigured        bool
	NotificationConfigured bool
	TaxConfigured          bool
	Pending                map[string]int64
	PendingError           string
	// C14：能力维度。可选未启用（disabled=未购买/被停用）与核心降级（degraded）
	// 分开表达，不把"未购买"渲染成健康故障；enabled 恒为配置校验通过后的运行态
	// （Load 对"开启但配置不全"失败关闭）。
	ContractIntegrationEnabled bool
	NativeFinancialEnabled     bool
	CreditSyncEnabled          bool
	ProjectReferenceConfigured bool
}

// destinationConfigured 判定某目的地本次是否有可投递配置。
func (s integrationSnapshot) destinationConfigured(name string) bool {
	switch name {
	case outbox.DestinationPlatformAudit:
		return s.AuditConfigured
	case outbox.DestinationPlatformNotification:
		return s.NotificationConfigured
	case "TAX_INVOICE_COMMAND":
		return s.TaxConfigured
	default:
		return false
	}
}

// unconfiguredPending 汇总“有积压但目的地未配置”的事件与目的地。
func (s integrationSnapshot) unconfiguredPending() (int64, []string) {
	var total int64
	names := make([]string, 0)
	for name, count := range s.Pending {
		if count <= 0 || s.destinationConfigured(name) {
			continue
		}
		total += count
		names = append(names, name)
	}
	sort.Strings(names)
	return total, names
}

// evaluateReadiness 返回 /readyz 的状态码与响应体。
// 参数：snapshot 为目的地配置与积压快照。
// 返回值：HTTP 状态码与只包含状态/计数的响应体；required 审计缺失返回 503。
func evaluateReadiness(snapshot integrationSnapshot) (int, map[string]any) {
	degraded := make([]string, 0, 3)
	if !snapshot.AuditConfigured {
		degraded = append(degraded, outbox.DestinationPlatformAudit)
	}
	if !snapshot.NotificationConfigured {
		degraded = append(degraded, outbox.DestinationPlatformNotification)
	}
	unconfiguredTotal, unconfiguredNames := snapshot.unconfiguredPending()
	degraded = append(degraded, unconfiguredNames...)
	body := map[string]any{
		"status": "ready",
		"integrations": map[string]any{
			"audit":        map[string]any{"required": snapshot.AuditRequired, "configured": snapshot.AuditConfigured},
			"notification": map[string]any{"required": false, "configured": snapshot.NotificationConfigured},
		},
		"capabilities": map[string]any{
			"contract_integration": map[string]any{"enabled": snapshot.ContractIntegrationEnabled, "state": capabilityState(snapshot.ContractIntegrationEnabled)},
			"native_financial":     map[string]any{"enabled": snapshot.NativeFinancialEnabled, "state": capabilityState(snapshot.NativeFinancialEnabled)},
			"crm_credit_sync":      map[string]any{"enabled": snapshot.CreditSyncEnabled, "state": capabilityState(snapshot.CreditSyncEnabled)},
			"project_reference":    map[string]any{"enabled": snapshot.ProjectReferenceConfigured, "state": capabilityState(snapshot.ProjectReferenceConfigured)},
		},
		"outbox_pending":              snapshot.Pending,
		"unconfigured_destinations":   unconfiguredNames,
		"unconfigured_outbox_pending": unconfiguredTotal,
	}
	if snapshot.PendingError != "" {
		body["outbox_pending_error"] = snapshot.PendingError
	}
	if snapshot.AuditRequired && !snapshot.AuditConfigured {
		body["status"] = "not_ready"
		body["reason"] = "审计目的地为必开但机器凭据不全（SETTLEMENT_AUDIT_CLIENT_ID/SECRET）"
		return 503, body
	}
	if snapshot.PendingError != "" || len(degraded) > 0 {
		body["status"] = "degraded"
		body["degraded_destinations"] = uniqueStrings(degraded)
	}
	return 200, body
}

// capabilityState 按 C14 口径渲染能力状态：disabled 属可选未购买/被停用，
// 不计入 degraded；enabled 表示配置校验通过后的运行态。
// 参数：enabled 为该能力开关的运行事实。
// 返回值：ready 或 disabled。
func capabilityState(enabled bool) string {
	if enabled {
		return "ready"
	}
	return "disabled"
}

// renderMetrics 把目的地配置与积压渲染为 Prometheus 文本格式。
// 参数：snapshot 为目的地配置与积压快照。
// 返回值：文本指标；未配置目的地计数单独暴露，避免“无目的地=不可见”。
func renderMetrics(snapshot integrationSnapshot) string {
	names := make([]string, 0, len(snapshot.Pending))
	for name := range snapshot.Pending {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	builder.WriteString("# HELP settlement_outbox_pending settlement outbox events not yet delivered, grouped by destination.\n")
	builder.WriteString("# TYPE settlement_outbox_pending gauge\n")
	for _, name := range names {
		fmt.Fprintf(&builder, "settlement_outbox_pending{destination=%q} %d\n", name, snapshot.Pending[name])
	}
	builder.WriteString("# HELP settlement_integration_destination_configured whether machine credentials for the destination are configured.\n")
	builder.WriteString("# TYPE settlement_integration_destination_configured gauge\n")
	for _, destination := range []string{outbox.DestinationPlatformAudit, outbox.DestinationPlatformNotification, "TAX_INVOICE_COMMAND"} {
		configured := 0
		if snapshot.destinationConfigured(destination) {
			configured = 1
		}
		fmt.Fprintf(&builder, "settlement_integration_destination_configured{destination=%q} %d\n", destination, configured)
	}
	builder.WriteString("# TYPE settlement_integration_audit_required gauge\n")
	builder.WriteString("settlement_integration_audit_required ")
	if snapshot.AuditRequired {
		builder.WriteString("1")
	} else {
		builder.WriteString("0")
	}
	builder.WriteString("\n")
	unconfiguredTotal, _ := snapshot.unconfiguredPending()
	builder.WriteString("# HELP settlement_outbox_unconfigured_pending pending events whose destination has no configured credentials.\n")
	builder.WriteString("# TYPE settlement_outbox_unconfigured_pending gauge\n")
	fmt.Fprintf(&builder, "settlement_outbox_unconfigured_pending %d\n", unconfiguredTotal)
	if snapshot.PendingError != "" {
		builder.WriteString("settlement_outbox_pending_query_error 1\n")
	} else {
		builder.WriteString("settlement_outbox_pending_query_error 0\n")
	}
	return builder.String()
}

// integrationSnapshot 读取当前进程的目的地配置与 outbox 积压。
// 参数：ctx 为查询上下文。
// 返回值：快照；数据库不可用时 PendingError 非空，不阻塞其它状态输出。
func (a *API) integrationSnapshot(ctx context.Context) integrationSnapshot {
	snapshot := integrationSnapshot{
		AuditRequired:              a.cfg.AuditDestinationRequired,
		AuditConfigured:            a.cfg.AuditDestinationConfigured,
		NotificationConfigured:     a.cfg.NotificationDestinationConfigured,
		TaxConfigured:              a.cfg.InvoiceIssuanceMode == "tax_adapter",
		ContractIntegrationEnabled: a.cfg.IntegrationEnabled,
		NativeFinancialEnabled:     a.cfg.NativeFinancialEnabled,
		CreditSyncEnabled:          a.cfg.CRMCreditSyncEnabled,
		ProjectReferenceConfigured: strings.TrimSpace(a.cfg.ProjectReferenceURL) != "",
		Pending:                    map[string]int64{},
	}
	if a.service == nil || a.service.DB == nil {
		snapshot.PendingError = "database unavailable"
		return snapshot
	}
	pending, err := (&outbox.Store{DB: a.service.DB}).PendingByDestination(ctx)
	if err != nil {
		snapshot.PendingError = err.Error()
		return snapshot
	}
	snapshot.Pending = pending
	return snapshot
}

// uniqueStrings 去重并保持首次出现顺序。
func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
