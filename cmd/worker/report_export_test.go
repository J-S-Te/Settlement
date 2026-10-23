package main

import "testing"

func TestReportExporterRequiresProvisionedTenantAndCarriesGatewayApplication(t *testing.T) {
	t.Setenv("OIDC_TENANT_ID", "")
	if w, err := newReportExporter(nil, "worker"); err == nil || w != nil {
		t.Fatal("worker accepted missing deployment tenant")
	}
	t.Setenv("OIDC_TENANT_ID", " tenant-a ")
	t.Setenv("SETTLEMENT_FILE_GATEWAY_MODE", "REQUIRED")
	t.Setenv("SETTLEMENT_FILE_GATEWAY_APPLICATION_ID", " application-a ")
	w, err := newReportExporter(nil, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if w.TenantID != "tenant-a" || w.GatewayApplicationID != "application-a" || w.GatewayMode != "required" {
		t.Fatalf("provisioned scope not wired into exporter: tenant=%q application=%q mode=%q", w.TenantID, w.GatewayApplicationID, w.GatewayMode)
	}
}
