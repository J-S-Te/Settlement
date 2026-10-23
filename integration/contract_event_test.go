package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/httpapi"
	"github.com/j-s-te/settlement/internal/service"
)

// TestContractEventIngestTenantBoundary 验证合同事件摄取的机器边界租户
// 一致性守卫（AUD-2026-001）：同租户
// 事件被接受（202），跨租户声明被拒绝，且
// 未认证请求绝不能通过机器边界。
//
// 与 TestInvoiceFlow 一样，此测试写数据库，需显式启用：
//
//	SETTLEMENT_E2E_TEST=1 SETTLEMENT_E2E_TEST_DSN='...' go test ./integration -v
func TestContractEventIngestTenantBoundary(t *testing.T) {
	if os.Getenv("SETTLEMENT_E2E_TEST") != "1" {
		t.Skip("set SETTLEMENT_E2E_TEST=1 to run the database-backed business flow")
	}
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_E2E_TEST_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("SETTLEMENT_MYSQL_DSN"))
	}
	if dsn == "" {
		t.Fatal("SETTLEMENT_E2E_TEST_DSN or SETTLEMENT_MYSQL_DSN is required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("database is not reachable: %v", err)
	}

	const tenant = "dev"
	const source = "integration-test"
	suffix := strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	acceptedEventID := "E2E-CE-" + suffix
	rejectedEventID := "E2E-CE-R-" + suffix
	revisionEventID := "E2E-C2-" + suffix
	contractID := "E2E-CE-CONTRACT-" + suffix
	defer cleanupContractEvent(t, db, tenant, source, acceptedEventID, contractID)
	defer cleanupContractEvent(t, db, tenant, source, revisionEventID, contractID)
	defer cleanupContractEvent(t, db, "other-tenant", source, rejectedEventID, contractID)

	handler := httpapi.New(db, config.Config{
		DevelopmentAuth: true, IntegrationEnabled: true, IntegrationBearerToken: "e2e-machine-secret",
		OIDCTenantID: tenant, PublicOrigin: "http://localhost:5173",
	}, slog.Default(), nil, nil, nil)

	event := func(eventID, declaredTenant string) map[string]any {
		return map[string]any{
			"event_id": eventID, "event_type": "contract.financial_effective.v1", "tenant_id": declaredTenant,
			"occurred_at": time.Now().UTC().Format(time.RFC3339),
			"aggregate":   map[string]any{"id": contractID, "version": 1},
			"contract": map[string]any{
				"id": contractID, "no": "E2E-CE-HT-" + suffix, "version": 1,
				"customer_id": "customer-e2e", "customer_name": "集成测试客户",
				"project_id": "project-e2e", "project_name": "集成测试项目",
				"currency": "CNY", "amount": "1000.00",
				"payment_terms": []map[string]any{{"installment_no": 1, "due_date": "2099-12-31", "amount": "1000.00"}},
			},
		}
	}

	unauthenticated := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", jsonBody(t, event(acceptedEventID, tenant)))
	unauthenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("contract event without machine token status=%d, want 401", unauthenticatedResponse.Code)
	}

	crossTenant := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", jsonBody(t, event(rejectedEventID, "other-tenant")))
	crossTenant.Header.Set("Authorization", "Bearer e2e-machine-secret")
	crossTenantResponse := httptest.NewRecorder()
	handler.ServeHTTP(crossTenantResponse, crossTenant)
	if crossTenantResponse.Code != http.StatusBadRequest || !strings.Contains(crossTenantResponse.Body.String(), "SETTLEMENT_INVALID_REQUEST") {
		t.Fatalf("cross-tenant contract event status=%d body=%s, want 400 SETTLEMENT_INVALID_REQUEST", crossTenantResponse.Code, crossTenantResponse.Body.String())
	}

	sameTenant := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", jsonBody(t, event(acceptedEventID, tenant)))
	sameTenant.Header.Set("Authorization", "Bearer e2e-machine-secret")
	sameTenantResponse := httptest.NewRecorder()
	handler.ServeHTTP(sameTenantResponse, sameTenant)
	if sameTenantResponse.Code != http.StatusAccepted {
		t.Fatalf("same-tenant contract event status=%d body=%s, want 202", sameTenantResponse.Code, sameTenantResponse.Body.String())
	}
	var envelope struct {
		Data struct {
			Accepted bool `json:"accepted"`
		} `json:"data"`
	}
	if err := json.NewDecoder(sameTenantResponse.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Data.Accepted {
		t.Fatalf("same-tenant contract event accepted=false, want true")
	}
	var snapshotCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_contract_snapshot WHERE tenant_id=? AND source_event_id=?`, tenant, acceptedEventID).Scan(&snapshotCount); err != nil || snapshotCount != 1 {
		t.Fatalf("same-tenant snapshot count=%d err=%v, want exactly one financial snapshot", snapshotCount, err)
	}
	// 先确认第一份计划，使修订反例覆盖真实
	// 应收，而非仅覆盖空计划。
	var planID string
	var planVersion int
	if err := db.QueryRow(`SELECT p.id,p.version FROM settlement_receivable_plan p JOIN settlement_contract_snapshot s ON s.id=p.contract_snapshot_id AND s.tenant_id=p.tenant_id WHERE p.tenant_id=? AND s.source_event_id=?`, tenant, acceptedEventID).Scan(&planID, &planVersion); err != nil {
		t.Fatal(err)
	}
	if err := (&service.Service{DB: db}).ConfirmPlan(ctx, service.Principal{TenantID: tenant, UserID: "e2e-finance", Permissions: map[string]bool{"settlement.admin": true}}, planID, service.PlanConfirmationInput{Version: planVersion}); err != nil {
		t.Fatalf("confirm first plan: %v", err)
	}
	var receivableCount int
	var receivableOpen string
	if err := db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(open_amount),0) FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=(SELECT id FROM settlement_contract_snapshot WHERE tenant_id=? AND source_event_id=?)`, tenant, tenant, acceptedEventID).Scan(&receivableCount, &receivableOpen); err != nil {
		t.Fatal(err)
	}
	if receivableCount != 1 || receivableOpen != "1000.00" {
		t.Fatalf("first receivable count=%d open=%s, want one 1000.00", receivableCount, receivableOpen)
	}

	// 精确事件重放为空操作：返回已接受结果，
	// 不得创建第二份快照或应收计划。
	replay := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", jsonBody(t, event(acceptedEventID, tenant)))
	replay.Header.Set("Authorization", "Bearer e2e-machine-secret")
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusAccepted || !strings.Contains(replayResponse.Body.String(), `"accepted":true`) || !strings.Contains(replayResponse.Body.String(), `"duplicate":true`) {
		t.Fatalf("exact replay status=%d body=%s, want 202 accepted=true duplicate=true", replayResponse.Code, replayResponse.Body.String())
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_contract_snapshot WHERE tenant_id=? AND source_event_id=?`, tenant, acceptedEventID).Scan(&snapshotCount); err != nil || snapshotCount != 1 {
		t.Fatalf("replay snapshot count=%d err=%v, want unchanged one snapshot", snapshotCount, err)
	}

	// 后续财务修订不得创建第二份全额应收。
	// 合同流守卫必须在写入任何财务事实前返回冲突。
	revision := event(revisionEventID, tenant)
	revisionContract := revision["contract"].(map[string]any)
	revisionContract["version"] = 2
	revisionContract["amount"] = "1200.00"
	revisionContract["payment_terms"] = []map[string]any{{"installment_no": 1, "due_date": "2099-12-31", "amount": "1200.00"}}
	revision["aggregate"].(map[string]any)["version"] = 2
	revisionRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", jsonBody(t, revision))
	revisionRequest.Header.Set("Authorization", "Bearer e2e-machine-secret")
	revisionResponse := httptest.NewRecorder()
	handler.ServeHTTP(revisionResponse, revisionRequest)
	if revisionResponse.Code != http.StatusConflict || !strings.Contains(revisionResponse.Body.String(), "SETTLEMENT_CONFLICT") {
		t.Fatalf("revision status=%d body=%s, want 409 SETTLEMENT_CONFLICT", revisionResponse.Code, revisionResponse.Body.String())
	}
	var revisionSnapshots, revisionPlans int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_contract_snapshot WHERE tenant_id=? AND source_event_id=?`, tenant, revisionEventID).Scan(&revisionSnapshots); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_receivable_plan p JOIN settlement_contract_snapshot s ON s.id=p.contract_snapshot_id AND s.tenant_id=p.tenant_id WHERE p.tenant_id=? AND s.source_event_id=?`, tenant, revisionEventID).Scan(&revisionPlans); err != nil {
		t.Fatal(err)
	}
	if revisionSnapshots != 0 || revisionPlans != 0 {
		t.Fatalf("revision wrote snapshots=%d plans=%d, want none", revisionSnapshots, revisionPlans)
	}
	if err := db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(open_amount),0) FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=(SELECT id FROM settlement_contract_snapshot WHERE tenant_id=? AND source_event_id=?)`, tenant, tenant, acceptedEventID).Scan(&receivableCount, &receivableOpen); err != nil {
		t.Fatal(err)
	}
	if receivableCount != 1 || receivableOpen != "1000.00" {
		t.Fatalf("revision changed receivables count=%d open=%s, want unchanged one 1000.00", receivableCount, receivableOpen)
	}
}

func cleanupContractEvent(t *testing.T, db *sql.DB, tenant, source, eventID, contractID string) {
	t.Helper()
	var snapshotID string
	_ = db.QueryRow(`SELECT id FROM settlement_contract_snapshot WHERE tenant_id=? AND source_event_id=?`, tenant, eventID).Scan(&snapshotID)
	if snapshotID != "" {
		if _, err := db.Exec(`DELETE FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID); err != nil {
			t.Errorf("cleanup receivables: %v", err)
		}
		if _, err := db.Exec(`DELETE FROM settlement_receivable_plan WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshotID); err != nil {
			t.Errorf("cleanup receivable plans: %v", err)
		}
		if _, err := db.Exec(`DELETE FROM settlement_contract_snapshot WHERE tenant_id=? AND id=?`, tenant, snapshotID); err != nil {
			t.Errorf("cleanup contract snapshot: %v", err)
		}
	}
	if _, err := db.Exec(`DELETE FROM settlement_inbox_event WHERE tenant_id=? AND source_application=? AND event_id=?`, tenant, source, eventID); err != nil {
		t.Errorf("cleanup inbox event: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM settlement_contract_stream WHERE tenant_id=? AND source_application=? AND source_contract_id=?`, tenant, source, contractID); err != nil {
		t.Errorf("cleanup contract stream: %v", err)
	}
}
