package outbox

import (
	"errors"
	"fmt"
	"strings"
)

// BIZ-XAPI-10：审计/通知机器凭据缺失时不能再静默跳过目的地装配。
// 本文件的纯函数把 worker 的启动装配变成可单测的判定，避免 os.Exit 无法覆盖。
const (
	// DestinationPlatformAudit 是平台审计批量写入目的地。
	DestinationPlatformAudit = "PLATFORM_AUDIT"
	// DestinationPlatformNotification 是平台站内信批量写入目的地。
	DestinationPlatformNotification = "PLATFORM_NOTIFICATION"
)

// ErrAuditDestinationRequired 表示生产/显式要求审计目的地，但机器凭据不全。
// worker 必须据此失败关闭（os.Exit(1)），不能带着“审计永远不投递”的进程继续运行。
var ErrAuditDestinationRequired = errors.New("audit destination is required but its machine credentials are incomplete")

// PlatformDestinationInput 是平台目的地装配所需的只读输入。
type PlatformDestinationInput struct {
	PlatformBaseURL          string
	EnvironmentCode          string
	ApplicationCode          string
	NotificationClientID     string
	NotificationClientSecret string
	AuditClientID            string
	AuditClientSecret        string
	AuditRequired            bool
}

// DegradedDestination 描述因凭据缺失而未装配的集成目的地。
// Reason 只允许写配置项名称与影响，不写入任何凭据值。
type DegradedDestination struct {
	Name   string
	Reason string
}

// PlatformDestinationPlan 是装配结果：可投递目的地、降级项与启动告警。
type PlatformDestinationPlan struct {
	Destinations []Destination
	Degraded     []DegradedDestination
	Warnings     []string
}

// PlanPlatformDestinations 按 BIZ-XAPI-10 分级策略装配平台目的地：
//   - 审计：凭据齐全才装配；required 且缺失 → 返回 ErrAuditDestinationRequired（失败关闭）；
//   - 审计：非 required 且缺失 → 只登记 degraded 与告警；
//   - 通知：缺失永不阻断启动，但必须登记 degraded（供 readyz/metrics/日志告警）。
//
// 参数：in 为平台地址、应用/环境编码与两组机器凭据（仅用于是否齐全的判定）。
// 返回值：装配结果；审计必开而缺失时返回包装后的 ErrAuditDestinationRequired。
func PlanPlatformDestinations(in PlatformDestinationInput) (PlatformDestinationPlan, error) {
	var plan PlatformDestinationPlan
	baseURL := strings.TrimRight(strings.TrimSpace(in.PlatformBaseURL), "/")
	applicationCode := strings.TrimSpace(in.ApplicationCode)
	if applicationCode == "" {
		applicationCode = "settlement"
	}
	environmentCode := strings.TrimSpace(in.EnvironmentCode)
	if environmentCode == "" {
		environmentCode = "dev"
	}
	notificationConfigured := strings.TrimSpace(in.NotificationClientID) != "" && strings.TrimSpace(in.NotificationClientSecret) != ""
	if notificationConfigured {
		plan.Destinations = append(plan.Destinations, Destination{
			Name: DestinationPlatformNotification, Endpoint: baseURL + "/api/v1/notifications/events/batch",
			Scope: "notification.ingest", ClientID: strings.TrimSpace(in.NotificationClientID),
			ClientSecret: in.NotificationClientSecret, ApplicationCode: applicationCode, EnvironmentCode: environmentCode,
		})
	} else {
		plan.Degraded = append(plan.Degraded, DegradedDestination{
			Name:   DestinationPlatformNotification,
			Reason: "SETTLEMENT_NOTIFICATION_CLIENT_ID/SETTLEMENT_NOTIFICATION_CLIENT_SECRET are incomplete; notification outbox events remain pending locally",
		})
		plan.Warnings = append(plan.Warnings, "settlement notification destination is not configured; local outbox events for PLATFORM_NOTIFICATION will accumulate until credentials are provided")
	}
	auditConfigured := strings.TrimSpace(in.AuditClientID) != "" && strings.TrimSpace(in.AuditClientSecret) != ""
	if auditConfigured {
		plan.Destinations = append(plan.Destinations, Destination{
			Name: DestinationPlatformAudit, Endpoint: baseURL + "/api/v1/audit/events/batch",
			Scope: "audit.ingest", ClientID: strings.TrimSpace(in.AuditClientID),
			ClientSecret: in.AuditClientSecret, ApplicationCode: applicationCode, EnvironmentCode: environmentCode,
		})
	} else if in.AuditRequired {
		return plan, fmt.Errorf("%w: SETTLEMENT_AUDIT_CLIENT_ID/SETTLEMENT_AUDIT_CLIENT_SECRET", ErrAuditDestinationRequired)
	} else {
		plan.Degraded = append(plan.Degraded, DegradedDestination{
			Name:   DestinationPlatformAudit,
			Reason: "SETTLEMENT_AUDIT_CLIENT_ID/SETTLEMENT_AUDIT_CLIENT_SECRET are incomplete while audit is not required; audit outbox events remain pending locally",
		})
		plan.Warnings = append(plan.Warnings, "settlement audit destination is not configured and audit is not required; local outbox events for PLATFORM_AUDIT will accumulate until credentials are provided")
	}
	return plan, nil
}

// UnconfiguredPending 统计“有积压但当前没有目的地可认领”的事件数。
// 参数：pending 为按目的地聚合的未完成事件数，destinations 为本次实际装配的目的地。
// 返回值：未装配目的地的积压计数；空结果返回空 map，避免调用方区分 nil。
// 安全理由：没有目的地的积压必须继续可见，不能因为不在遍历列表里就当作不存在。
func UnconfiguredPending(pending map[string]int64, destinations []Destination) map[string]int64 {
	configured := make(map[string]struct{}, len(destinations))
	for _, destination := range destinations {
		configured[destination.Name] = struct{}{}
	}
	result := make(map[string]int64)
	for name, count := range pending {
		if count <= 0 {
			continue
		}
		if _, ok := configured[name]; ok {
			continue
		}
		result[name] = count
	}
	return result
}
