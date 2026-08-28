package platform

import "testing"

func TestSettlementCatalogCompatibilityAcceptsNMinusOneAndRejectsPartialWindow(t *testing.T) {
	value := authorizationContext{CatalogVersion: "2", CompatibleCatalogVersions: []string{"2", "1"}, RoleConfigHash: "hash-2", CompatibleRoleConfigHashes: []string{"hash-2", "settlement-v1-online-authorization"}}
	if err := validateSettlementCatalogCompatibility(value); err != nil {
		t.Fatalf("N-1 catalog rejected: %v", err)
	}
	value.CompatibleCatalogVersions = nil
	if err := validateSettlementCatalogCompatibility(value); err == nil {
		t.Fatal("partial compatibility response accepted")
	}
}
