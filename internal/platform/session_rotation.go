package platform

import (
	"context"
	"encoding/json"
	"errors"
	provider "github.com/coreos/go-oidc/v3/oidc"
	"github.com/j-s-te/settlement/internal/service"
	"golang.org/x/oauth2"
	"time"
)

// rotateCentralCredentials 只轮换并核验原身份凭据，不查询业务权限。
// 参数：ctx为有界轮换上下文，row为数据库锁内最新会话。
// 返回值：最新密文和变更标记；明确拒绝与临时故障分别返回错误。
func (a *Authenticator) rotateCentralCredentials(ctx context.Context, row centralSession) (refreshedCentralSession, bool, error) {
	updated := refreshedCentralSession{centralSession: row}
	plain, err := a.decrypt(row.TokenCipher)
	if err != nil {
		return updated, false, ErrUnauthenticated
	}
	var token oauth2.Token
	if json.Unmarshal(plain, &token) != nil || token.AccessToken == "" {
		return updated, false, ErrUnauthenticated
	}
	changed := false
	if !token.Expiry.After(time.Now().UTC().Add(time.Minute)) {
		if token.RefreshToken == "" {
			return updated, false, ErrUnauthenticated
		}
		refreshed, err := a.oauth.TokenSource(provider.ClientContext(ctx, a.client), &oauth2.Token{RefreshToken: token.RefreshToken, Expiry: time.Now().Add(-time.Minute)}).Token()
		if err != nil {
			var protocol *oauth2.RetrieveError
			if errors.As(err, &protocol) && (protocol.ErrorCode == "invalid_grant" || protocol.ErrorCode == "invalid_token") {
				return updated, false, ErrUnauthenticated
			}
			return updated, false, ErrSessionUnavailable
		}
		if refreshed.AccessToken == "" || !refreshed.Expiry.After(time.Now().UTC()) {
			return updated, false, ErrSessionUnavailable
		}
		if refreshed.RefreshToken == "" {
			refreshed.RefreshToken = token.RefreshToken
		}
		if raw, ok := refreshed.Extra("id_token").(string); ok && raw != "" {
			id, err := a.verifier.Verify(provider.ClientContext(ctx, a.client), raw)
			if err != nil {
				return updated, false, ErrUnauthenticated
			}
			var claims oidcClaims
			if id.Claims(&claims) != nil || claims.TokenUse != "id_token" || claims.IdentityID != row.IdentityID || claims.Subject != row.Subject || claims.SessionID != row.SID || claims.TenantID != row.TenantID {
				return updated, false, ErrUnauthenticated
			}
			updated.IDCipher = a.encrypt([]byte(raw))
		}
		token = *refreshed
		updated.TokenCipher = a.encrypt(mustJSON(&token))
		changed = true
	}
	updated.TokenExpiresAt = token.Expiry.UTC()
	return updated, changed, nil
}

// authorizeCentralCredentials 使用第一事务已提交的凭据核验当前业务权限，不再次刷新。
// 参数：ctx为原请求上下文，row为第二事务重读的最新会话。
// 返回值：当前业务主体及凭据；权限或依赖失败时不授予业务访问。
func (a *Authenticator) authorizeCentralCredentials(ctx context.Context, row centralSession) (service.Principal, refreshedCentralSession, error) {
	updated := refreshedCentralSession{centralSession: row}
	plain, err := a.decrypt(row.TokenCipher)
	if err != nil {
		return service.Principal{}, updated, ErrUnauthenticated
	}
	var token oauth2.Token
	if json.Unmarshal(plain, &token) != nil || token.AccessToken == "" {
		return service.Principal{}, updated, ErrUnauthenticated
	}
	value, err := a.resolveAuthorization(ctx, token.AccessToken)
	if err != nil {
		return service.Principal{}, updated, err
	}
	subject, subjectErr := canonicalSubjectID(value.SubjectID, value.IdentityID)
	if subjectErr != nil || subject != row.IdentityID || value.Subject != row.Subject || value.TenantID != row.TenantID || value.ClientID != a.cfg.OIDCClientID || value.ApplicationCode != "settlement" || value.EnvironmentCode != a.cfg.OIDCEnvironmentCode || value.AuthorizationRevision == 0 || validateSettlementCatalogCompatibility(value) != nil {
		return service.Principal{}, updated, ErrUnauthenticated
	}
	var stored service.Principal
	if json.Unmarshal(row.PrincipalJSON, &stored) != nil {
		return service.Principal{}, updated, ErrUnauthenticated
	}
	current := principal(value, oidcClaims{IdentityID: row.IdentityID, PersonID: stored.PersonID, Name: stored.DisplayName, PreferredUsername: stored.Username})
	updated.TokenExpiresAt = token.Expiry.UTC()
	return current, updated, nil
}
