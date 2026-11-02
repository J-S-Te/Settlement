package platform

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"
)

// ClientCredentialsVerifierOptions 定义交接机器协议唯一接受的平台信任域。
// 与合同/项目仓库的平台验签器同名同形，避免四端对同一枚令牌出现两套解释。
type ClientCredentialsVerifierOptions struct {
	Issuer, Audience, PublicKeyPath, ClientID, TenantID string
	CallerApplicationCode, CallerEnvironmentCode        string
	RequiredScope                                       string
}

// ClientCredentialsTokenVerifier 只加载平台 Ed25519 公钥；业务进程不持有平台签发私钥。
// 它与 OIDC 会话验签有意隔离：平台 OIDC JWKS 使用另一套签名管理器，不能验证应用令牌。
type ClientCredentialsTokenVerifier struct {
	publicKey                        ed25519.PublicKey
	issuer, audience                 string
	clientID, tenantID               string
	applicationCode, environmentCode string
	requiredScope                    string
}

type clientCredentialsClaims struct {
	Issuer          string   `json:"iss"`
	Audience        string   `json:"aud"`
	TokenUse        string   `json:"token_use"`
	Subject         string   `json:"sub"`
	OAuthClientID   string   `json:"oauth_client_id"`
	TenantID        string   `json:"tenant_id"`
	ApplicationID   string   `json:"application_id"`
	ApplicationCode string   `json:"application_code"`
	EnvironmentID   string   `json:"environment_id"`
	EnvironmentCode string   `json:"environment_code"`
	Scopes          []string `json:"scope"`
	IssuedAt        int64    `json:"iat"`
	NotBefore       int64    `json:"nbf"`
	ExpiresAt       int64    `json:"exp"`
}

// NewClientCredentialsTokenVerifier 从只读公钥构建平台 application JWT 验签器。
// 缺项、带空白或公钥无效时返回错误；成功时返回失败关闭的本地验签器。
func NewClientCredentialsTokenVerifier(_ context.Context, options ClientCredentialsVerifierOptions) (*ClientCredentialsTokenVerifier, error) {
	for _, value := range []string{options.Issuer, options.Audience, options.PublicKeyPath, options.ClientID, options.TenantID, options.CallerApplicationCode, options.CallerEnvironmentCode, options.RequiredScope} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("%w: machine token trust binding is incomplete", ErrInvalidServiceToken)
		}
	}
	publicKey, err := loadClientCredentialsPublicKey(options.PublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidServiceToken, err)
	}
	return &ClientCredentialsTokenVerifier{
		publicKey: publicKey, issuer: options.Issuer, audience: options.Audience,
		clientID: options.ClientID, tenantID: options.TenantID,
		applicationCode: options.CallerApplicationCode, environmentCode: options.CallerEnvironmentCode,
		requiredScope: options.RequiredScope,
	}, nil
}

// Verify 校验紧凑序列化、固定算法头、签名，以及 issuer/audience/token_use、调用方
// 应用与环境、唯一 scope 和租户绑定；任一不符即失败关闭。租户只来自签名 claims。
func (v *ClientCredentialsTokenVerifier) Verify(_ context.Context, rawToken string) (ServiceIdentity, error) {
	deny := func() (ServiceIdentity, error) { return ServiceIdentity{}, ErrInvalidServiceToken }
	if v == nil || len(rawToken) > 16384 {
		return deny()
	}
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return deny()
	}
	header, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return deny()
	}
	var h struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if strictJSONObject(header, &h) != nil || h.Alg != "EdDSA" || h.Typ != "JWT" {
		return deny()
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(v.publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return deny()
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return deny()
	}
	var c clientCredentialsClaims
	if strictJSONObject(payload, &c) != nil {
		return deny()
	}
	now := time.Now().UTC()
	if c.Issuer != v.issuer || c.Audience != v.audience || c.TokenUse != "application" || c.Subject != v.clientID || c.OAuthClientID == "" {
		return deny()
	}
	if c.TenantID != v.tenantID || c.ApplicationCode != v.applicationCode || c.EnvironmentCode != v.environmentCode {
		return deny()
	}
	// 此用途只有一个 scope。即使由共享平台密钥签名，额外或重复 scope 也不构成交接令牌。
	if len(c.Scopes) != 1 || c.Scopes[0] != v.requiredScope || c.IssuedAt <= 0 || c.NotBefore < c.IssuedAt || c.ExpiresAt <= c.NotBefore {
		return deny()
	}
	if time.Unix(c.NotBefore, 0).After(now.Add(time.Minute)) || !time.Unix(c.ExpiresAt, 0).After(now) {
		return deny()
	}
	return ServiceIdentity{TenantID: c.TenantID, ApplicationCode: c.ApplicationCode, EnvironmentCode: c.EnvironmentCode}, nil
}

func loadClientCredentialsPublicKey(path string) (ed25519.PublicKey, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, remainder := pem.Decode(contents)
	if block == nil || len(bytes.TrimSpace(remainder)) != 0 {
		return nil, fmt.Errorf("application JWT public key must contain exactly one PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("application JWT public key must be an Ed25519 PKIX key")
	}
	return publicKey, nil
}
