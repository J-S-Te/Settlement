package platform

import (
	"errors"
	"net"
	"strings"

	"github.com/j-s-te/settlement/internal/service"
)

// oidcClaims 是浏览器 OIDC ID 令牌 的最小身份投影。
// 权限不从这些 Claims 推导，必须由 authorizationContext 提供。
type oidcClaims struct {
	Subject           string `json:"sub"`
	IdentityID        string `json:"identity_id"`
	TenantID          string `json:"tenant_id"`
	PersonID          string `json:"person_id"`
	Nonce             string `json:"nonce"`
	TokenUse          string `json:"token_use"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

// principal 用途：将可信在线授权转换为保留角色和范围的结算主体。
// 参数：a 为平台授权，claims 为已验证的身份显示信息。
// 返回值：返回业务主体，不从身份令牌推断业务权限。
func principal(a authorizationContext, claims oidcClaims) service.Principal {
	permissions := make(map[string]bool, len(a.Permissions))
	for _, permission := range a.Permissions {
		permissions[permission] = true
	}
	// 没有可信角色时不能把旧平台缺失元数据解释为全租户财务授权。
	if len(a.Roles) == 0 {
		permissions = map[string]bool{}
	}
	subjectID, _ := canonicalSubjectID(a.SubjectID, a.IdentityID)
	scopes := make([]service.DataScope, 0, len(a.DataScopes))
	for _, scope := range a.DataScopes {
		scopes = append(scopes, service.DataScope{RoleCode: scope.RoleCode, ScopeType: scope.ScopeType, ScopeID: scope.ScopeID, EnvironmentCode: scope.EnvironmentCode})
	}
	version := a.CatalogVersion
	if version == "" {
		version = "online-unversioned"
	}
	return service.Principal{
		TenantID: a.TenantID, UserID: subjectID, Permissions: permissions,
		Roles: append([]string(nil), a.Roles...), DataScopes: scopes,
		IdentityID: a.IdentityID, PersonID: claims.PersonID,
		DisplayName: strings.TrimSpace(claims.Name), Username: strings.TrimSpace(claims.PreferredUsername),
		CatalogVersion: version, AuthorizationRevision: a.AuthorizationRevision,
		LoginIP: normalizeIP(a.UserLoginIP),
	}
}

func canonicalSubjectID(subjectID, identityID string) (string, error) {
	subjectID = strings.TrimSpace(subjectID)
	identityID = strings.TrimSpace(identityID)
	if subjectID == "" {
		subjectID = identityID
	}
	if subjectID == "" || identityID == "" || subjectID != identityID {
		return "", errors.New("subject_id and identity_id must identify the same platform subject")
	}
	return subjectID, nil
}

// normalizeIP 规范化登录地址；非法值返回空，避免把脏数据写进审计。
func normalizeIP(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed := net.ParseIP(value); parsed != nil {
		return parsed.String()
	}
	return ""
}
