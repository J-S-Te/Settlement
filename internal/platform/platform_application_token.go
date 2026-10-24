package platform

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/j-s-te/settlement/internal/config"
)

// PlatformApplicationBinding 是已注册的源合同客户端，绝非
// 接收端 Settlement 应用 UID。仅加载公钥材料。
type PlatformApplicationBinding struct {
	PublicKeyPath, Issuer, ClientID, OAuthClientID, Audience, TenantID, ApplicationID, EnvironmentID, EnvironmentCode string
}
type platformApplicationVerifier struct {
	publicKey ed25519.PublicKey
	binding   PlatformApplicationBinding
}

// NewContractServiceTokenVerifier 选择显式平台信任域。
// 平台配置损坏或不完整时，不能回退到 OIDC。
func NewContractServiceTokenVerifier(ctx context.Context, c config.Config) (*ServiceTokenVerifier, error) {
	if c.PlatformMachineTokenPublicKeyPath != "" || c.PlatformMachineTokenIssuer != "" || c.ContractOAuthClientID != "" || c.ContractApplicationID != "" || c.ContractEnvironmentID != "" {
		return NewPlatformApplicationTokenVerifier(PlatformApplicationBinding{c.PlatformMachineTokenPublicKeyPath, c.PlatformMachineTokenIssuer, c.ContractClientID, c.ContractOAuthClientID, c.ContractAudience, c.OIDCTenantID, c.ContractApplicationID, c.ContractEnvironmentID, c.OIDCEnvironmentCode})
	}
	return NewServiceTokenVerifier(ctx, c.OIDCIssuer, c.ContractClientID, c.ContractAudience, c.OIDCTenantID, "contract_management", c.OIDCEnvironmentCode, "settlement.contract.ingest")
}
func NewPlatformApplicationTokenVerifier(binding PlatformApplicationBinding) (*ServiceTokenVerifier, error) {
	for _, value := range []string{binding.PublicKeyPath, binding.Issuer, binding.ClientID, binding.OAuthClientID, binding.Audience, binding.TenantID, binding.ApplicationID, binding.EnvironmentID, binding.EnvironmentCode} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("platform application token binding is incomplete")
		}
	}
	body, err := os.ReadFile(binding.PublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read platform machine public key: %w", err)
	}
	block, rest := pem.Decode(body)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("platform machine key must be one PUBLIC KEY PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse platform machine public key: %w", err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("platform machine key must be Ed25519")
	}
	return &ServiceTokenVerifier{applicationVerifier: &platformApplicationVerifier{key, binding}}, nil
}
func (v *platformApplicationVerifier) verify(raw string, now time.Time) (ServiceIdentity, error) {
	deny := func() (ServiceIdentity, error) { return ServiceIdentity{}, ErrInvalidServiceToken }
	if len(raw) > 16384 {
		return deny()
	}
	parts := strings.Split(raw, ".")
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
	var c struct {
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
	if strictJSONObject(payload, &c) != nil {
		return deny()
	}
	b := v.binding
	if c.Issuer != b.Issuer || c.Audience != b.Audience || c.TokenUse != "application" || c.Subject != b.ClientID || c.OAuthClientID != b.OAuthClientID || c.TenantID != b.TenantID || c.ApplicationID != b.ApplicationID || c.ApplicationCode != "contract_management" || c.EnvironmentID != b.EnvironmentID || c.EnvironmentCode != b.EnvironmentCode {
		return deny()
	}
	// 此用途仅有一个范围。即使由共享平台密钥签名，额外
	// 或重复范围也不构成合同摄取令牌。
	if len(c.Scopes) != 1 || c.Scopes[0] != "settlement.contract.ingest" || c.IssuedAt <= 0 || c.NotBefore < c.IssuedAt || c.ExpiresAt <= c.NotBefore || c.IssuedAt > now.Unix() || c.NotBefore > now.Unix() || c.ExpiresAt <= now.Unix() {
		return deny()
	}
	return ServiceIdentity{c.TenantID, c.ApplicationCode, c.EnvironmentCode}, nil
}

// 声明为扁平对象。解码前拒绝重复键，不能
// 接受 encoding/json 对签名声明采用的末值覆盖解释。
func strictJSONObject(body []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return ErrInvalidServiceToken
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || name != strings.ToLower(name) || seen[name] {
			return ErrInvalidServiceToken
		}
		seen[name] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
	}
	if _, err = d.Token(); err != nil {
		return err
	}
	if _, err = d.Token(); err != io.EOF {
		return ErrInvalidServiceToken
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}
