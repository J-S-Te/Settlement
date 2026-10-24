package platform

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/config"
)

func machineFixture(t *testing.T) (config.Config, ed25519.PrivateKey, map[string]any) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "public.pem")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	c := config.Config{PlatformMachineTokenPublicKeyPath: path, PlatformMachineTokenIssuer: "basic-platform", ContractClientID: "contract_management-prod-settlement-integration", ContractOAuthClientID: "oauth-contract", ContractAudience: "basic-platform-application", OIDCTenantID: "tenant", ContractApplicationID: "app-contract", ContractEnvironmentID: "env-contract-prod", OIDCEnvironmentCode: "prod", OIDCIssuer: "http://unreachable.invalid"}
	now := time.Now().Unix()
	claims := map[string]any{"iss": "basic-platform", "aud": "basic-platform-application", "token_use": "application", "sub": c.ContractClientID, "oauth_client_id": c.ContractOAuthClientID, "tenant_id": "tenant", "application_id": "app-contract", "application_code": "contract_management", "environment_id": "env-contract-prod", "environment_code": "prod", "scope": []string{"settlement.contract.ingest"}, "iat": now - 2, "nbf": now - 2, "exp": now + 600}
	return c, private, claims
}
func signedMachine(t *testing.T, key ed25519.PrivateKey, header, payload any) string {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	p, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return signedMachineBytes(key, h, p)
}
func signedMachineBytes(key ed25519.PrivateKey, header, payload []byte) string {
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
}
func TestProductionContractVerifierAcceptsRegisteredPlatformApplicationToken(t *testing.T) {
	c, key, claims := machineFixture(t)
	v, err := NewContractServiceTokenVerifier(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := v.Verify(context.Background(), signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims))
	if err != nil || identity.TenantID != "tenant" || identity.ApplicationCode != "contract_management" || identity.EnvironmentCode != "prod" {
		t.Fatalf("registered machine token: identity=%+v error=%v", identity, err)
	}
}
func TestPlatformApplicationVerifierRejectsWrongTrustBinding(t *testing.T) {
	mutations := map[string]any{"iss": "other", "aud": "settlement", "token_use": "access_token", "sub": "other-client", "oauth_client_id": "other-uid", "tenant_id": "other-tenant", "application_id": "settlement-app", "application_code": "settlement", "environment_id": "other-env", "environment_code": "dev", "scope": []string{"platform:file:upload"}, "iat": time.Now().Unix() + 60, "nbf": time.Now().Unix() + 60, "exp": time.Now().Unix()}
	for field, value := range mutations {
		t.Run(field, func(t *testing.T) {
			c, key, claims := machineFixture(t)
			claims[field] = value
			v, err := NewContractServiceTokenVerifier(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = v.Verify(context.Background(), signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims)); !errors.Is(err, ErrInvalidServiceToken) {
				t.Fatal("wrong binding accepted")
			}
		})
	}
	for _, field := range []string{"iss", "aud", "token_use", "sub", "oauth_client_id", "tenant_id", "application_id", "application_code", "environment_id", "environment_code", "scope", "iat", "nbf", "exp"} {
		t.Run("missing_"+field, func(t *testing.T) {
			c, key, claims := machineFixture(t)
			delete(claims, field)
			v, _ := NewContractServiceTokenVerifier(context.Background(), c)
			if _, err := v.Verify(context.Background(), signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims)); err == nil {
				t.Fatal("missing binding accepted")
			}
		})
	}
}
func TestPlatformApplicationVerifierRejectsAmbiguousJWTAndSignature(t *testing.T) {
	c, key, claims := machineFixture(t)
	v, err := NewContractServiceTokenVerifier(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	header := []byte(`{"alg":"EdDSA","typ":"JWT"}`)
	_, foreignKey, _ := ed25519.GenerateKey(rand.Reader)
	bad := map[string]string{
		"foreign_signature": signedMachineBytes(foreignKey, header, payload),
		"duplicate_header":  signedMachineBytes(key, []byte(`{"alg":"none","alg":"EdDSA","typ":"JWT"}`), payload),
		"case_alias_claim":  signedMachineBytes(key, header, append([]byte(`{"TOKEN_USE":"access_token",`), payload[1:]...)),
		"duplicate_claim":   signedMachineBytes(key, header, append([]byte(`{"token_use":"access_token",`), payload[1:]...)),
		"extra_header":      signedMachineBytes(key, []byte(`{"alg":"EdDSA","typ":"JWT","crit":["b64"]}`), payload),
		"trailing_json":     signedMachineBytes(key, header, append(payload, []byte(` {}`)...)),
		"none_alg":          signedMachineBytes(key, []byte(`{"alg":"none","typ":"JWT"}`), payload),
		"wrong_typ":         signedMachineBytes(key, []byte(`{"alg":"EdDSA","typ":"at+jwt"}`), payload),
		"oversized":         strings.Repeat("a", 16385),
	}
	for _, scopes := range [][]string{{"settlement.contract.ingest", "settlement.contract.ingest"}, {"settlement.contract.ingest", "platform:file:upload"}, {}} {
		claims["scope"] = scopes
		bad["scope_"+strings.Join(scopes, "_")] = signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims)
	}
	claims["scope"] = "settlement.contract.ingest"
	bad["string_scope"] = signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims)
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), raw); !errors.Is(err, ErrInvalidServiceToken) {
				t.Fatal("ambiguous JWT accepted")
			}
		})
	}
}
func TestPlatformModeCannotFallBackToOIDC(t *testing.T) {
	requests := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; http.Error(w, "must not reach", 500) }))
	defer provider.Close()
	c, _, _ := machineFixture(t)
	c.OIDCIssuer = provider.URL
	c.PlatformMachineTokenPublicKeyPath = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := NewContractServiceTokenVerifier(context.Background(), c); err == nil {
		t.Fatal("invalid platform key accepted")
	}
	if requests != 0 {
		t.Fatal("platform mode fell back to OIDC")
	}
	c.PlatformMachineTokenPublicKeyPath = ""
	if _, err := NewContractServiceTokenVerifier(context.Background(), c); err == nil {
		t.Fatal("partial platform config accepted")
	}
	if requests != 0 {
		t.Fatal("partial platform mode fell back")
	}
}
func TestLegacyContractAndTaxConstructorsKeepOIDCDiscovery(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/auth", "token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	defer server.Close()
	c := config.Config{OIDCIssuer: server.URL, ContractClientID: "legacy", ContractAudience: "aud", OIDCTenantID: "tenant", OIDCEnvironmentCode: "prod"}
	legacy, err := NewContractServiceTokenVerifier(context.Background(), c)
	if err != nil || legacy.verifier == nil || legacy.applicationVerifier != nil {
		t.Fatalf("legacy constructor changed: %v", err)
	}
	tax, err := NewStrictServiceTokenVerifier(context.Background(), server.URL, "tax", "aud", "tenant", "tax_adapter", "prod", "settlement.tax.result.ingest")
	if err != nil || !tax.strict || tax.applicationVerifier != nil {
		t.Fatalf("tax constructor changed: %v", err)
	}
}
