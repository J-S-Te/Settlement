package platform

import "testing"

func TestSettlementCatalogCompatibilityAcceptsNMinusOneAndRejectsPartialWindow(t *testing.T) {
	value := authorizationContext{CatalogVersion: "3", CompatibleCatalogVersions: []string{"3", "2"}, RoleConfigHash: "hash-3", CompatibleRoleConfigHashes: []string{"hash-3", "settlement-v2-invoice-read"}}
	if err := validateSettlementCatalogCompatibility(value); err != nil {
		t.Fatalf("N-1 catalog rejected: %v", err)
	}
	value.CompatibleCatalogVersions = nil
	if err := validateSettlementCatalogCompatibility(value); err == nil {
		t.Fatal("partial compatibility response accepted")
	}
}
