package platform

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// handoverFixture 复刻平台 ApplicationJWTManager 为交接协议签发的 application JWT：
// caller=platform、env=prod、client_id=platform-handover、唯一 scope=handover.machine。
func handoverFixture(t *testing.T) (ClientCredentialsVerifierOptions, ed25519.PrivateKey, map[string]any) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "application-jwt-public.pem")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	options := ClientCredentialsVerifierOptions{
		Issuer: "basic-platform", Audience: "basic-platform-application", PublicKeyPath: path,
		ClientID: "platform-handover", TenantID: "tenant",
		CallerApplicationCode: "platform", CallerEnvironmentCode: "prod", RequiredScope: "handover.machine",
	}
	now := time.Now().Unix()
	claims := map[string]any{
		"iss": "basic-platform", "aud": "basic-platform-application", "token_use": "application",
		"sub": "platform-handover", "oauth_client_id": "oauth-platform-handover", "tenant_id": "tenant",
		"application_id": "app-platform", "application_code": "platform",
		"environment_id": "env-platform-prod", "environment_code": "prod",
		"scope": []string{"handover.machine"}, "iat": now - 2, "nbf": now - 2, "exp": now + 600,
	}
	return options, private, claims
}

func TestClientCredentialsVerifierAcceptsPlatformHandoverToken(t *testing.T) {
	options, key, claims := handoverFixture(t)
	verifier, err := NewClientCredentialsTokenVerifier(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verifier.Verify(context.Background(), signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if identity.TenantID != "tenant" || identity.ApplicationCode != "platform" || identity.EnvironmentCode != "prod" {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestClientCredentialsVerifierRejectsWrongClaimsOrTrustBinding(t *testing.T) {
	mutations := map[string]any{
		"iss": "other-issuer", "aud": "settlement", "token_use": "access_token", "sub": "contract-ingest",
		"tenant_id": "other-tenant", "application_code": "contract_management", "environment_code": "dev",
		"scope": []string{"settlement.contract.ingest"},
		"iat":   int64(0),
		"nbf":   time.Now().Unix() - 10,
		"exp":   time.Now().Unix() - 60,
	}
	for field, value := range mutations {
		t.Run(field, func(t *testing.T) {
			options, key, claims := handoverFixture(t)
			claims[field] = value
			verifier, err := NewClientCredentialsTokenVerifier(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = verifier.Verify(context.Background(), signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims)); !errors.Is(err, ErrInvalidServiceToken) {
				t.Fatalf("claims drift on %s accepted", field)
			}
		})
	}
	// 严格解码：绑定字段缺失等同漂移，不允许“缺项即跳过比较”。
	for _, field := range []string{"iss", "aud", "token_use", "sub", "oauth_client_id", "tenant_id", "application_code", "environment_code", "scope", "iat", "nbf", "exp"} {
		t.Run("missing_"+field, func(t *testing.T) {
			options, key, claims := handoverFixture(t)
			delete(claims, field)
			verifier, err := NewClientCredentialsTokenVerifier(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = verifier.Verify(context.Background(), signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims)); !errors.Is(err, ErrInvalidServiceToken) {
				t.Fatalf("missing %s accepted", field)
			}
		})
	}
	// 接收端绑定漂移同样失败关闭：caller 应用/环境与 scope 由配置固定，不接受令牌自述。
	optionMutations := map[string]func(*ClientCredentialsVerifierOptions){
		"caller_application": func(o *ClientCredentialsVerifierOptions) { o.CallerApplicationCode = "data_analysis" },
		"caller_environment": func(o *ClientCredentialsVerifierOptions) { o.CallerEnvironmentCode = "dev" },
		"required_scope":     func(o *ClientCredentialsVerifierOptions) { o.RequiredScope = "settlement.contract.ingest" },
		"client_id":          func(o *ClientCredentialsVerifierOptions) { o.ClientID = "contract-ingest" },
		"tenant":             func(o *ClientCredentialsVerifierOptions) { o.TenantID = "other-tenant" },
	}
	for name, mutate := range optionMutations {
		t.Run("option_"+name, func(t *testing.T) {
			options, key, claims := handoverFixture(t)
			mutate(&options)
			verifier, err := NewClientCredentialsTokenVerifier(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = verifier.Verify(context.Background(), signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims)); !errors.Is(err, ErrInvalidServiceToken) {
				t.Fatalf("trust binding drift on %s accepted", name)
			}
		})
	}
}

func TestClientCredentialsVerifierRejectsAmbiguousOrForgedJWT(t *testing.T) {
	options, key, claims := handoverFixture(t)
	verifier, err := NewClientCredentialsTokenVerifier(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	header := []byte(`{"alg":"EdDSA","typ":"JWT"}`)
	_, foreignKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	invalid := map[string]string{
		"foreign_signature": signedMachineBytes(foreignKey, header, payload),
		"duplicate_claim":   signedMachineBytes(key, header, append([]byte(`{"token_use":"access_token",`), payload[1:]...)),
		"unknown_claim":     signedMachineBytes(key, header, append([]byte(`{"jti":"x",`), payload[1:]...)),
		"trailing_json":     signedMachineBytes(key, header, append(payload, []byte(` {}`)...)),
		"none_algorithm":    signedMachineBytes(key, []byte(`{"alg":"none","typ":"JWT"}`), payload),
		"empty":             "",
		"two_parts":         "header.payload",
	}
	for name, token := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), token); !errors.Is(err, ErrInvalidServiceToken) {
				t.Fatalf("invalid token %s accepted", name)
			}
		})
	}
	var nilVerifier *ClientCredentialsTokenVerifier
	if _, err := nilVerifier.Verify(context.Background(), signedMachine(t, key, header, claims)); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatal("nil verifier accepted")
	}
}

func TestNewClientCredentialsTokenVerifierRejectsIncompleteBinding(t *testing.T) {
	for name, mutate := range map[string]func(*ClientCredentialsVerifierOptions){
		"issuer":         func(o *ClientCredentialsVerifierOptions) { o.Issuer = "" },
		"audience":       func(o *ClientCredentialsVerifierOptions) { o.Audience = "" },
		"public_key":     func(o *ClientCredentialsVerifierOptions) { o.PublicKeyPath = "" },
		"client_id":      func(o *ClientCredentialsVerifierOptions) { o.ClientID = "" },
		"tenant":         func(o *ClientCredentialsVerifierOptions) { o.TenantID = "" },
		"caller_app":     func(o *ClientCredentialsVerifierOptions) { o.CallerApplicationCode = "" },
		"caller_env":     func(o *ClientCredentialsVerifierOptions) { o.CallerEnvironmentCode = "" },
		"required_scope": func(o *ClientCredentialsVerifierOptions) { o.RequiredScope = "" },
	} {
		t.Run(name, func(t *testing.T) {
			options, _, _ := handoverFixture(t)
			mutate(&options)
			if _, err := NewClientCredentialsTokenVerifier(context.Background(), options); !errors.Is(err, ErrInvalidServiceToken) {
				t.Fatalf("incomplete binding %s accepted", name)
			}
		})
	}
	options, _, _ := handoverFixture(t)
	options.PublicKeyPath = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := NewClientCredentialsTokenVerifier(context.Background(), options); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatal("missing public key accepted")
	}
}
