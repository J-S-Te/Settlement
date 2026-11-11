package authz

import (
	"bytes"
	"encoding/json"
	"testing"
)

// platformCatalogPermission 对齐平台应用目录的严格权限字段协议。
type platformCatalogPermission struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Action       string `json:"action"`
	ResourceCode string `json:"resource_code"`
	ResourceName string `json:"resource_name,omitempty"`
	RiskLevel    string `json:"risk_level,omitempty"`
	GrantPolicy  string `json:"grant_policy,omitempty"`
}

// TestEmbeddedManifestMatchesPlatformWireContract 防止内嵌目录被平台严格解码拒绝。
// 参数：t为测试上下文，输入为实际发布的内嵌权限目录。
// 返回值：无；出现平台不支持的字段或财务风险等级错误时测试失败。
func TestEmbeddedManifestMatchesPlatformWireContract(t *testing.T) {
	var manifest struct {
		CatalogVersion       string                      `json:"catalog_version"`
		Checksum             string                      `json:"checksum,omitempty"`
		SourceType           string                      `json:"source_type,omitempty"`
		SourceIdentifier     string                      `json:"source_identifier,omitempty"`
		ClaimsRoleConfigHash string                      `json:"claims_role_config_hash"`
		Permissions          []platformCatalogPermission `json:"permissions"`
		Roles                []struct {
			Code        string   `json:"code"`
			Name        string   `json:"name"`
			Description string   `json:"description,omitempty"`
			Permissions []string `json:"permissions"`
		} `json:"roles"`
		Policy struct {
			MaxEffectiveRoles int `json:"max_effective_roles"`
		} `json:"policy"`
	}
	decoder := json.NewDecoder(bytes.NewReader(PermissionManifest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("平台目录协议拒绝实际内嵌清单: %v", err)
	}
	want := map[string]string{"read": "LOW", "record": "MEDIUM", "review": "HIGH"}
	for _, permission := range manifest.Permissions {
		if permission.ResourceCode != "recognition" {
			continue
		}
		if permission.ResourceName != "财务确认" || permission.RiskLevel != want[permission.Action] {
			t.Errorf("财务确认权限展示或风险等级不匹配: %s", permission.Code)
		}
		delete(want, permission.Action)
	}
	if len(want) != 0 {
		t.Fatalf("缺少财务确认动作: %v", want)
	}
}
