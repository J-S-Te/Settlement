// Package catalog 定义可登记到签名授权的商业运行组件有限集合。
// 目录编译进平台、Agent 及每个消费端构建，
// 不作为可由客户编辑的配置文件提供。
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Version 仅在组件契约变化时更新。常规镜像重建不改变版本，
// 因此不需要更新目录。
const Version = 1

// RegistrationProtocolV1 表示旧版镜像与覆盖范围批准协议。
// RegistrationProtocolV2 按身份登记目录组件。
const (
	RegistrationProtocolV1 = 1
	RegistrationProtocolV2 = 2
)

// RegistrationMode 由平台按应用持久化。桥接模式表示升级进度，
// 不豁免商业授权检查。
type RegistrationMode string

const (
	LegacyImageV1       RegistrationMode = "LEGACY_IMAGE_V1"
	BridgingComponentV2 RegistrationMode = "BRIDGING_COMPONENT_V2"
	BuiltinComponentV2  RegistrationMode = "BUILTIN_COMPONENT_V2"
)

// 运行时角色仅保留必要类别。基础设施组件（目前仅 Metabase）
// 需要安装和监控，但不发送业务 ACK。
const (
	RoleAPI            = "API"
	RoleWorker         = "WORKER"
	RoleInfrastructure = "INFRASTRUCTURE"
)

var (
	ErrUnknownApplication = errors.New("catalog application is unknown")
	ErrUnknownComponent   = errors.New("catalog component is unknown")
	ErrInvalidSelection   = errors.New("catalog selection is invalid")
)

// Component 表示一个允许的运行时服务。RegistrationProtocol 指定目录可登记的协议，
// 签名快照协议仍为 1。
type Component struct {
	Application          string
	ServiceID            string
	Role                 string
	Conditional          bool
	RegistrationProtocol int
}

var componentsByApplication = map[string][]Component{
	"contract_management": {
		{Application: "contract_management", ServiceID: "contract-api", Role: RoleAPI, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "contract_management", ServiceID: "contract-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
	},
	"customer_and_opportunity": {
		{Application: "customer_and_opportunity", ServiceID: "customer-api", Role: RoleAPI, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "customer_and_opportunity", ServiceID: "customer-opportunity-alert-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "customer_and_opportunity", ServiceID: "customer-owner-notification-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "customer_and_opportunity", ServiceID: "customer-contract-transfer-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "customer_and_opportunity", ServiceID: "customer-presale-alert-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "customer_and_opportunity", ServiceID: "customer-presale-assignment-notification-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "customer_and_opportunity", ServiceID: "customer-presale-progress-notification-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "customer_and_opportunity", ServiceID: "customer-notification-delivery-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "customer_and_opportunity", ServiceID: "customer-presale-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
	},
	"customer_portal": {
		{Application: "customer_portal", ServiceID: "portal-api", Role: RoleAPI, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "customer_portal", ServiceID: "portal-report-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "customer_portal", ServiceID: "portal-invite-compensation-worker", Role: RoleWorker, Conditional: true, RegistrationProtocol: RegistrationProtocolV2},
	},
	"data_analysis": {
		{Application: "data_analysis", ServiceID: "data-analysis-api", Role: RoleAPI, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "data_analysis", ServiceID: "data-analysis-aggregation-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "data_analysis", ServiceID: "data-analysis-alert-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "data_analysis", ServiceID: "data-analysis-metabase", Role: RoleInfrastructure, RegistrationProtocol: RegistrationProtocolV2},
	},
	"project_management": {
		{Application: "project_management", ServiceID: "project-api", Role: RoleAPI, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "project_management", ServiceID: "project-sla-notifier", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
	},
	"settlement": {
		{Application: "settlement", ServiceID: "settlement-api", Role: RoleAPI, RegistrationProtocol: RegistrationProtocolV2},
		{Application: "settlement", ServiceID: "settlement-worker", Role: RoleWorker, RegistrationProtocol: RegistrationProtocolV2},
	},
}

// Components 返回指定应用的有限组件目录副本。
func Components(application string) ([]Component, error) {
	components, ok := componentsByApplication[application]
	if !ok {
		return nil, ErrUnknownApplication
	}
	return append([]Component(nil), components...), nil
}

// Lookup 仅在所属应用内查找组件。属于其他应用的服务将被拒绝，
// 不会回退到全局搜索。
func Lookup(application, serviceID string) (Component, error) {
	components, ok := componentsByApplication[application]
	if !ok {
		return Component{}, ErrUnknownApplication
	}
	for _, component := range components {
		if component.ServiceID == serviceID {
			return component, nil
		}
	}
	return Component{}, ErrUnknownComponent
}

// ClientIDV2 根据部署实例与目录身份派生稳定的机器身份。
// 派生过程不包含镜像、覆盖范围及发布摘要，
// 以便常规重建沿用原有凭据与 ACK 身份。
func ClientIDV2(instanceID, application, environment, serviceID, credentialGeneration string) string {
	raw, _ := json.Marshal([]any{instanceID, application, environment, serviceID, credentialGeneration})
	digest := sha256.Sum256(raw)
	prefix := application + "-" + environment + "-license-" + serviceID
	if len(prefix) > 95 {
		prefix = prefix[:95]
	}
	return prefix + "-" + hex.EncodeToString(digest[:16])
}

// ValidateSelection 拒绝未知、重复、跨应用或不完整的组件选择。
// 允许省略条件组件和基础设施组件，
// 但必须包含所有非条件消费组件。
func ValidateSelection(application string, selected []string) error {
	components, ok := componentsByApplication[application]
	if !ok {
		return ErrUnknownApplication
	}
	allowed := make(map[string]Component, len(components))
	for _, component := range components {
		allowed[component.ServiceID] = component
	}
	seen := make(map[string]struct{}, len(selected))
	for _, serviceID := range selected {
		if serviceID == "" {
			return fmt.Errorf("%w: empty service id", ErrInvalidSelection)
		}
		if _, exists := seen[serviceID]; exists {
			return fmt.Errorf("%w: duplicate service id %s", ErrInvalidSelection, serviceID)
		}
		if _, exists := allowed[serviceID]; !exists {
			return fmt.Errorf("%w: service %s is not in application %s", ErrUnknownComponent, serviceID, application)
		}
		seen[serviceID] = struct{}{}
	}
	for _, component := range components {
		if component.Role == RoleInfrastructure || component.Conditional {
			continue
		}
		if _, exists := seen[component.ServiceID]; !exists {
			return fmt.Errorf("%w: required service %s is missing", ErrInvalidSelection, component.ServiceID)
		}
	}
	return nil
}
