package reportexport

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
)

func TestUnconfiguredDeploymentTenantCannotGenerateOrClaim(t *testing.T) {
	d := &exportDB{}
	db := sql.OpenDB(d)
	defer db.Close()
	gateway := &recordingGateway{}
	w := &Worker{DB: db, WorkerID: "worker", Gateway: gateway, GatewayMode: "required", GatewayApplicationID: "settlement"}
	if err := w.generate(context.Background(), job{id: "job-1", tenantID: "foreign-tenant"}); err == nil {
		t.Error("missing deployment tenant allowed generation")
	}
	if _, err := w.claim(context.Background()); err == nil {
		t.Error("missing deployment tenant allowed claim")
	}
	if gateway.uploads != 0 || gateway.binds != 0 || len(d.queries) != 0 || len(d.execs) != 0 {
		t.Fatalf("unconfigured tenant reached SQL or gateway: uploads=%d binds=%d queries=%d execs=%d", gateway.uploads, gateway.binds, len(d.queries), len(d.execs))
	}
}

func TestForeignTenantCannotGenerateOrCompensate(t *testing.T) {
	d := &exportDB{}
	db := sql.OpenDB(d)
	defer db.Close()
	gateway := &recordingGateway{}
	w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker", Gateway: gateway, GatewayMode: "required", GatewayApplicationID: "settlement"}
	foreign := job{id: "job-1", tenantID: "foreign-tenant", generation: 7}
	if err := w.generate(context.Background(), foreign); err == nil {
		t.Error("foreign generation accepted")
	}
	if err := w.reconcileEffect(context.Background(), foreign); err == nil {
		t.Error("foreign compensation accepted")
	}
	if gateway.uploads != 0 || gateway.binds != 0 || gateway.releases != 0 || len(d.queries) != 0 || len(d.execs) != 0 {
		t.Fatal("foreign tenant reached SQL or gateway")
	}
}

func TestClaimAndCompensationQueriesUseDeploymentTenant(t *testing.T) {
	claims, compensations := 0, 0
	d := &exportDB{query: func(q string, args []driver.NamedValue) (driver.Rows, error) {
		if len(args) == 0 || args[0].Value != "tenant" {
			t.Fatalf("tenant not bound in query: %s", q)
		}
		if strings.Contains(q, "SELECT e.job_id") {
			compensations++
			if !strings.Contains(q, "WHERE e.tenant_id=?") {
				t.Fatal("compensation SQL is not tenant scoped")
			}
			return &exportRows{columns: 3}, nil
		}
		claims++
		if !strings.Contains(q, "WHERE tenant_id=?") {
			t.Fatal("claim SQL is not tenant scoped")
		}
		return &exportRows{columns: 4}, nil
	}}
	db := sql.OpenDB(d)
	defer db.Close()
	w := &Worker{DB: db, TenantID: "tenant"}
	if _, err := w.claim(context.Background()); err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if err := w.reconcileEffects(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if claims != 1 || compensations != 1 {
		t.Fatalf("query paths not exercised: claims=%d compensations=%d", claims, compensations)
	}
}

func TestCompensationRejectsAnotherGatewayApplication(t *testing.T) {
	d := &exportDB{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
		if strings.Contains(q, "SELECT status,lease_generation") {
			return &exportRows{columns: 4, values: [][]driver.Value{{"FAILED", int64(7), "", false}}}, nil
		}
		return &exportRows{columns: 6, values: [][]driver.Value{{"foreign-application", "request", "export.csv", []byte("private tenant report"), "", "INTENT"}}}, nil
	}}
	db := sql.OpenDB(d)
	defer db.Close()
	gateway := &recordingGateway{}
	w := &Worker{DB: db, TenantID: "tenant", GatewayApplicationID: "settlement", Gateway: gateway}
	if err := w.reconcileEffect(context.Background(), job{id: "job-1", tenantID: "tenant", generation: 7}); err == nil {
		t.Fatal("foreign gateway application accepted")
	}
	if gateway.uploads != 0 || gateway.releases != 0 {
		t.Fatal("foreign application report reached gateway")
	}
}
