package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

var ErrInvalidServiceToken = errors.New("invalid service token")

type ServiceIdentity struct{ TenantID, ApplicationCode, EnvironmentCode string }
type ServiceTokenVerifier struct {
	verifier                                                    *oidc.IDTokenVerifier
	clientID, audience, tenant, application, environment, scope string
	strict                                                      bool
}

func NewStrictServiceTokenVerifier(ctx context.Context, issuer, clientID, audience, tenant, application, environment, scope string) (*ServiceTokenVerifier, error) {
	verifier, err := NewServiceTokenVerifier(ctx, issuer, clientID, audience, tenant, application, environment, scope)
	if err == nil {
		verifier.strict = true
	}
	return verifier, err
}

func NewServiceTokenVerifier(ctx context.Context, issuer, clientID, audience, tenant, application, environment, scope string) (*ServiceTokenVerifier, error) {
	provider, err := oidc.NewProvider(ctx, strings.TrimRight(issuer, "/"))
	if err != nil {
		return nil, err
	}
	return &ServiceTokenVerifier{verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true}), clientID: clientID, audience: audience, tenant: tenant, application: application, environment: environment, scope: scope}, nil
}
func (v *ServiceTokenVerifier) Verify(ctx context.Context, raw string) (ServiceIdentity, error) {
	token, err := v.verifier.Verify(ctx, strings.TrimSpace(raw))
	if err != nil {
		return ServiceIdentity{}, ErrInvalidServiceToken
	}
	var c struct {
		AZP         string `json:"azp"`
		Type        string `json:"typ"`
		TokenUse    string `json:"token_use"`
		Tenant      string `json:"tenant_id"`
		Application string `json:"application_code"`
		Environment string `json:"environment_code"`
		Scope       string `json:"scope"`
	}
	claimsErr := token.Claims(&c)
	missingStrictClaims := v.strict && (c.Tenant == "" || c.Application == "" || c.Environment == "" || c.TokenUse == "")
	if claimsErr != nil || missingStrictClaims || !strings.EqualFold(c.Type, "bearer") || c.AZP != v.clientID || !audience(token.Audience, v.audience) || (c.TokenUse != "" && c.TokenUse != "access_token") || (c.Tenant != "" && c.Tenant != v.tenant) || (c.Application != "" && c.Application != v.application) || (c.Environment != "" && c.Environment != v.environment) || !hasScope(c.Scope, v.scope) {
		return ServiceIdentity{}, fmt.Errorf("%w: claims", ErrInvalidServiceToken)
	}
	return ServiceIdentity{v.tenant, v.application, v.environment}, nil
}
func hasScope(raw, expected string) bool {
	for _, value := range strings.Fields(raw) {
		if value == expected {
			return true
		}
	}
	return false
}
func audience(values []string, expected string) bool {
	for _, v := range values {
		if v == expected {
			return true
		}
	}
	return false
}
