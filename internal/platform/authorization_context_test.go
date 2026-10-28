package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
)

// TestSettlementCatalogCompatibilityAcceptsNMinusOneAndRejectsPartialWindow 用途：校验目录升级兼容窗口。
// 参数：t为测试上下文。
// 返回值：无，N/N-1兼容或残缺窗口拒绝行为异常时失败。
func TestSettlementCatalogCompatibilityAcceptsNMinusOneAndRejectsPartialWindow(t *testing.T) {
	value := authorizationContext{CatalogVersion: "4", CompatibleCatalogVersions: []string{"4", "3"}, RoleConfigHash: "hash-4", CompatibleRoleConfigHashes: []string{"hash-4", "settlement-v3-business-roles-scopes"}}
	if err := validateSettlementCatalogCompatibility(value); err != nil {
		t.Fatalf("N-1 catalog rejected: %v", err)
	}
	value.CompatibleCatalogVersions = nil
	if err := validateSettlementCatalogCompatibility(value); err == nil {
		t.Fatal("partial compatibility response accepted")
	}
}

func TestResolveAuthorizationAcceptsCompletePlatformContextContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/oauth2/authorization-context" || request.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("unexpected authorization context request: path=%q authorization=%q", request.URL.Path, request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"sub": "keycloak-subject", "subject_id": "platform-user", "identity_id": "platform-user",
			"tenant_id": "tenant", "client_id": "settlement-prod-web", "application_code": "settlement", "environment_code": "prod",
			"person_id": "person", "roles": []string{"settlement_admin"}, "permissions": []string{"settlement.invoice.read"},
			"data_scopes":     []map[string]string{{"role_code": "settlement_admin", "scope_type": "TENANT", "scope_id": "", "environment_code": "prod"}},
			"catalog_version": "2", "compatible_catalog_versions": []string{"2"},
			"role_config_hash": "settlement-v2-invoice-read", "compatible_role_config_hashes": []string{"settlement-v2-invoice-read"},
			"authorization_revision": 2, "user_login_ip": "127.0.0.1", "customer_ref": "customer-1",
		})
	}))
	defer server.Close()

	authenticator := &Authenticator{cfg: config.Config{PlatformBaseURL: server.URL}, client: server.Client()}
	resolved, err := authenticator.resolveAuthorization(context.Background(), "access-token")
	if err != nil {
		t.Fatalf("complete platform authorization context was rejected: %v", err)
	}
	if resolved.IdentityID != "platform-user" || resolved.PersonID != "person" || len(resolved.Roles) != 1 || len(resolved.DataScopes) != 1 {
		t.Fatalf("unexpected decoded authorization context: %#v", resolved)
	}
}
