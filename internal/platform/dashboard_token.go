package platform

import (
	"context"
	"encoding/base64"
	"strings"
	"time"
)

// DashboardTokenVerifier 将结算看板用途与注册OAuth客户端绑定。
type DashboardTokenVerifier struct {
	verifier      *ClientCredentialsTokenVerifier
	oauthClientID string
}

// NewDashboardTokenVerifier 构建独立结算看板机器身份验证器。
// 参数：ctx为初始化上下文，options绑定平台信任域，oauthClientID为注册记录ID。
// 返回值：严格验证器；用途、调用方或注册客户端缺失时返回身份错误。
func NewDashboardTokenVerifier(ctx context.Context, options ClientCredentialsVerifierOptions, oauthClientID string) (*DashboardTokenVerifier, error) {
	if options.CallerApplicationCode != "data_analysis" || options.RequiredScope != "dashboard.settlement.read" || oauthClientID == "" || strings.TrimSpace(oauthClientID) != oauthClientID {
		return nil, ErrInvalidServiceToken
	}
	verifier, err := NewClientCredentialsTokenVerifier(ctx, options)
	if err != nil {
		return nil, err
	}
	return &DashboardTokenVerifier{verifier: verifier, oauthClientID: oauthClientID}, nil
}

// Verify 在既有验签后严格绑定注册客户端与当前生效区间。
// 参数：ctx为请求上下文，raw为平台签发的紧凑application JWT。
// 返回值：可信租户身份；注册记录、用途或时间边界漂移时失败关闭。
func (v *DashboardTokenVerifier) Verify(ctx context.Context, raw string) (ServiceIdentity, error) {
	if v == nil || v.verifier == nil {
		return ServiceIdentity{}, ErrInvalidServiceToken
	}
	identity, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return ServiceIdentity{}, err
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(strings.Split(raw, ".")[1])
	if err != nil {
		return ServiceIdentity{}, ErrInvalidServiceToken
	}
	var claims clientCredentialsClaims
	now := time.Now().Unix()
	if strictJSONObject(payload, &claims) != nil || claims.OAuthClientID != v.oauthClientID || claims.IssuedAt > now || claims.NotBefore > now || claims.ExpiresAt <= now {
		return ServiceIdentity{}, ErrInvalidServiceToken
	}
	return identity, nil
}
