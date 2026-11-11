package platform

import (
	"context"
	"testing"
	"time"
)

// TestDashboardVerifierPinsRegisteredMachineIdentity 验证独立用途与客户端注册绑定。
// 参数：t为测试上下文。
// 返回值：无，跨用途、客户端或有效期漂移被接受时失败。
func TestDashboardVerifierPinsRegisteredMachineIdentity(t *testing.T) {
	options, key, claims := handoverFixture(t)
	options.ClientID = "data-analysis-dashboard"
	options.CallerApplicationCode = "data_analysis"
	options.RequiredScope = "dashboard.settlement.read"
	claims["sub"] = options.ClientID
	claims["application_code"] = "data_analysis"
	claims["scope"] = []string{"dashboard.settlement.read"}
	claims["oauth_client_id"] = "registered-oauth-id"
	verifier, err := NewDashboardTokenVerifier(context.Background(), options, "registered-oauth-id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifier.Verify(context.Background(), signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims)); err != nil {
		t.Fatal(err)
	}
	for field, bad := range map[string]any{
		"oauth_client_id": "other-client", "sub": "other-subject", "tenant_id": "other-tenant",
		"application_code": "contract_management", "environment_code": "other-env",
		"iss": "other-issuer", "aud": "other-audience", "token_use": "id_token",
		"scope": []string{"dashboard.settlement.read", "handover.machine"},
		"iat":   time.Now().Unix() + 10, "nbf": time.Now().Unix() + 10, "exp": time.Now().Unix() - 1,
	} {
		t.Run(field, func(t *testing.T) {
			old := claims[field]
			claims[field] = bad
			defer func() { claims[field] = old }()
			if _, err := verifier.Verify(context.Background(), signedMachine(t, key, map[string]string{"alg": "EdDSA", "typ": "JWT"}, claims)); err == nil {
				t.Fatalf("accepted %s drift", field)
			}
		})
	}
	options.CallerApplicationCode = "contract_management"
	if _, err := NewDashboardTokenVerifier(context.Background(), options, "registered-oauth-id"); err == nil {
		t.Fatal("accepted other application")
	}
}
