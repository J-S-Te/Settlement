package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/j-s-te/settlement/authz"
)

// authorizationContext 是基础平台依据 Access Token 返回的、结算系统专属授权上下文。
// 该结构只在平台适配边界内使用，结算业务不直接解析基础平台响应。
type authorizationContext struct {
	Subject                    string                   `json:"sub"`
	SubjectID                  string                   `json:"subject_id"`
	IdentityID                 string                   `json:"identity_id"`
	TenantID                   string                   `json:"tenant_id"`
	ClientID                   string                   `json:"client_id"`
	ApplicationCode            string                   `json:"application_code"`
	EnvironmentCode            string                   `json:"environment_code"`
	PersonID                   string                   `json:"person_id"`
	Roles                      []string                 `json:"roles"`
	Permissions                []string                 `json:"permissions"`
	DataScopes                 []authorizationDataScope `json:"data_scopes"`
	CatalogVersion             string                   `json:"catalog_version"`
	CompatibleCatalogVersions  []string                 `json:"compatible_catalog_versions"`
	RoleConfigHash             string                   `json:"role_config_hash"`
	CompatibleRoleConfigHashes []string                 `json:"compatible_role_config_hashes"`
	AuthorizationRevision      uint64                   `json:"authorization_revision"`
	UserLoginIP                string                   `json:"user_login_ip,omitempty"`
	CustomerRef                string                   `json:"customer_ref,omitempty"`
}

type authorizationDataScope struct {
	RoleCode        string `json:"role_code"`
	ScopeType       string `json:"scope_type"`
	ScopeID         string `json:"scope_id"`
	EnvironmentCode string `json:"environment_code"`
}

func validateSettlementCatalogCompatibility(value authorizationContext) error {
	var local struct {
		CatalogVersion string `json:"catalog_version"`
		RoleConfigHash string `json:"claims_role_config_hash"`
	}
	if err := json.Unmarshal(authz.PermissionManifest, &local); err != nil || strings.TrimSpace(local.CatalogVersion) == "" || strings.TrimSpace(local.RoleConfigHash) == "" {
		return errors.New("settlement embedded authorization catalog is invalid")
	}
	allMissing := value.CatalogVersion == "" && len(value.CompatibleCatalogVersions) == 0 && value.RoleConfigHash == "" && len(value.CompatibleRoleConfigHashes) == 0
	if allMissing {
		return nil
	}
	if value.CatalogVersion == "" || value.RoleConfigHash == "" || len(value.CompatibleCatalogVersions) == 0 || len(value.CompatibleCatalogVersions) > 2 || len(value.CompatibleRoleConfigHashes) == 0 || len(value.CompatibleRoleConfigHashes) > 2 {
		return errors.New("authorization catalog compatibility window is incomplete")
	}
	if !settlementCatalogValuePresent(value.CompatibleCatalogVersions, value.CatalogVersion) || !settlementCatalogValuePresent(value.CompatibleCatalogVersions, local.CatalogVersion) || !settlementCatalogValuePresent(value.CompatibleRoleConfigHashes, value.RoleConfigHash) || !settlementCatalogValuePresent(value.CompatibleRoleConfigHashes, local.RoleConfigHash) {
		return errors.New("settlement catalog is outside the N/N-1 compatibility window")
	}
	return nil
}

func settlementCatalogValuePresent(values []string, expected string) bool {
	seen := map[string]struct{}{}
	found := false
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
		found = found || value == expected
	}
	return found
}

// resolveAuthorization 调用基础平台授权上下文接口，验证令牌对应的应用、环境和权限。
// 非 200 响应、响应体过大或字段不符合契约时均返回错误，不使用请求头权限信息兜底。
func (a *Authenticator) resolveAuthorization(ctx context.Context, token string) (authorizationContext, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.cfg.PlatformBaseURL, "/")+"/oauth2/authorization-context", nil)
	if err != nil {
		return authorizationContext{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.client.Do(req)
	if err != nil {
		return authorizationContext{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return authorizationContext{}, fmt.Errorf("authorization HTTP %d", resp.StatusCode)
	}
	var value authorizationContext
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return authorizationContext{}, err
	}
	return value, nil
}
