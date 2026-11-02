package outbox

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

func platformDestinationInput(auditRequired bool) PlatformDestinationInput {
	return PlatformDestinationInput{
		PlatformBaseURL:          "http://platform-api:8080/",
		EnvironmentCode:          "prod",
		ApplicationCode:          "settlement",
		NotificationClientID:     "settlement-notification",
		NotificationClientSecret: "notification-secret",
		AuditClientID:            "settlement-audit",
		AuditClientSecret:        "audit-secret",
		AuditRequired:            auditRequired,
	}
}

func TestPlanPlatformDestinationsIncludesConfiguredIntegrations(t *testing.T) {
	plan, err := PlanPlatformDestinations(platformDestinationInput(true))
	if err != nil {
		t.Fatalf("PlanPlatformDestinations() error = %v", err)
	}
	if len(plan.Destinations) != 2 || len(plan.Degraded) != 0 || len(plan.Warnings) != 0 {
		t.Fatalf("plan = %+v, want two configured destinations without degradation", plan)
	}
	names := []string{plan.Destinations[0].Name, plan.Destinations[1].Name}
	if !reflect.DeepEqual(names, []string{DestinationPlatformNotification, DestinationPlatformAudit}) {
		t.Fatalf("destination names = %v", names)
	}
	if plan.Destinations[1].Endpoint != "http://platform-api:8080/api/v1/audit/events/batch" || plan.Destinations[1].ApplicationCode != "settlement" {
		t.Fatalf("audit destination = %+v", plan.Destinations[1])
	}
}

func TestPlanPlatformDestinationsFailsClosedWhenRequiredAuditMissing(t *testing.T) {
	input := platformDestinationInput(true)
	input.AuditClientID = ""
	plan, err := PlanPlatformDestinations(input)
	if !errors.Is(err, ErrAuditDestinationRequired) {
		t.Fatalf("error = %v, want ErrAuditDestinationRequired", err)
	}
	for _, destination := range plan.Destinations {
		if destination.Name == DestinationPlatformAudit {
			t.Fatal("required audit destination must not be assembled without credentials")
		}
	}
}

func TestPlanPlatformDestinationsWarnsWhenOptionalAuditMissing(t *testing.T) {
	input := platformDestinationInput(false)
	input.AuditClientID = ""
	input.AuditClientSecret = ""

	plan, err := PlanPlatformDestinations(input)
	if err != nil {
		t.Fatalf("PlanPlatformDestinations() error = %v", err)
	}
	if len(plan.Degraded) != 1 || plan.Degraded[0].Name != DestinationPlatformAudit {
		t.Fatalf("degraded = %+v, want only PLATFORM_AUDIT", plan.Degraded)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "audit destination is not configured") {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
	if len(plan.Destinations) != 1 || plan.Destinations[0].Name != DestinationPlatformNotification {
		t.Fatalf("destinations = %+v, want notification only", plan.Destinations)
	}
}

func TestPlanPlatformDestinationsNeverFailsForMissingNotification(t *testing.T) {
	input := platformDestinationInput(true)
	input.NotificationClientID = ""
	input.NotificationClientSecret = ""

	plan, err := PlanPlatformDestinations(input)
	if err != nil {
		t.Fatalf("missing notification must not fail startup: %v", err)
	}
	if len(plan.Degraded) != 1 || plan.Degraded[0].Name != DestinationPlatformNotification {
		t.Fatalf("degraded = %+v, want only PLATFORM_NOTIFICATION", plan.Degraded)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "notification destination is not configured") {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
	if len(plan.Destinations) != 1 || plan.Destinations[0].Name != DestinationPlatformAudit {
		t.Fatalf("destinations = %+v, want audit only", plan.Destinations)
	}
}

// TestUnconfiguredPendingKeepsOrphanBacklogVisible 覆盖“无目的地=不可见”的缺口：
// 有积压但没有装配目的地的目的地必须单独计数。
func TestUnconfiguredPendingKeepsOrphanBacklogVisible(t *testing.T) {
	pending := map[string]int64{
		DestinationPlatformAudit:        2,
		DestinationPlatformNotification: 3,
		"TAX_INVOICE_COMMAND":           4,
		"UNKNOWN_DESTINATION":           0,
	}
	unconfigured := UnconfiguredPending(pending, []Destination{{Name: DestinationPlatformAudit}})
	want := map[string]int64{DestinationPlatformNotification: 3, "TAX_INVOICE_COMMAND": 4}
	if !reflect.DeepEqual(unconfigured, want) {
		t.Fatalf("UnconfiguredPending() = %v, want %v", unconfigured, want)
	}
	if result := UnconfiguredPending(nil, nil); result == nil || len(result) != 0 {
		t.Fatalf("UnconfiguredPending(nil,nil) = %v, want empty non-nil map", result)
	}
}

func TestStorePendingByDestinationGroupsUnfinishedEvents(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{
		Kind: "query", SQL: "GROUP BY destination",
		Rows: [][]driver.Value{
			{DestinationPlatformAudit, int64(2)},
			{DestinationPlatformNotification, int64(5)},
		},
	})
	pending, err := (&Store{DB: db}).PendingByDestination(context.Background())
	if err != nil {
		t.Fatalf("PendingByDestination() error = %v", err)
	}
	want := map[string]int64{DestinationPlatformAudit: 2, DestinationPlatformNotification: 5}
	if !reflect.DeepEqual(pending, want) {
		t.Fatalf("pending = %v, want %v", pending, want)
	}
}

// TestWorkerRefreshBacklogRecordsOrphanDestinations 证明 worker 会把未配置目的地的
// 积压写进可观测字段，而不是只在遍历已装配目的地时静默跳过。
func TestWorkerRefreshBacklogRecordsOrphanDestinations(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{
		Kind: "query", SQL: "GROUP BY destination",
		Rows: [][]driver.Value{
			{DestinationPlatformAudit, int64(1)},
			{"TAX_INVOICE_COMMAND", int64(3)},
		},
	})
	worker := &Worker{
		Store:        &Store{DB: db, WorkerID: "worker-1", Lease: time.Minute},
		Destinations: []Destination{{Name: DestinationPlatformAudit}},
	}
	if err := worker.refreshBacklog(context.Background()); err != nil {
		t.Fatalf("refreshBacklog() error = %v", err)
	}
	want := map[string]int64{"TAX_INVOICE_COMMAND": 3}
	if !reflect.DeepEqual(worker.UnconfiguredPending, want) {
		t.Fatalf("UnconfiguredPending = %v, want %v", worker.UnconfiguredPending, want)
	}
}
