package platform

import (
	"errors"
	"strings"

	"github.com/j-s-te/settlement/internal/service"
)

// oidcClaims 是浏览器 OIDC ID Token 的最小身份投影。
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

// principal 将已通过平台授权上下文校验的身份和权限转换为结算业务使用的主体。
func principal(a authorizationContext, claims oidcClaims) service.Principal {
	permissions := make(map[string]bool, len(a.Permissions))
	for _, permission := range a.Permissions {
		permissions[permission] = true
	}
	subjectID, _ := canonicalSubjectID(a.SubjectID, a.IdentityID)
	return service.Principal{
		TenantID: a.TenantID, UserID: subjectID, Permissions: permissions,
		IdentityID: a.IdentityID, PersonID: claims.PersonID,
		DisplayName: strings.TrimSpace(claims.Name), Username: strings.TrimSpace(claims.PreferredUsername),
		CatalogVersion: a.CatalogVersion, AuthorizationRevision: a.AuthorizationRevision,
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
