package outbox

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestBackoffIsBounded(t *testing.T) {
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour}
	for i, expected := range want {
		if got := backoff(i + 1); got != expected {
			t.Fatalf("attempt %d: got %s want %s", i+1, got, expected)
		}
	}
	if got := backoff(100); got != 6*time.Hour {
		t.Fatalf("backoff is not capped: %s", got)
	}
}

func TestAuditPayloadIsNormalizedForPlatform(t *testing.T) {
	event := Event{EventID: "ABCDEF0123456789ABCDEF0123456789", EventType: "SETTLEMENT_TEST", AggregateType: "invoice", AggregateID: "inv-1", CreatedAt: time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC), Payload: json.RawMessage(`{"actor_id":"user-1","result":"SUCCESS","risk_level":"HIGH"}`)}
	body, err := payloadFor(Destination{Name: "PLATFORM_AUDIT", ApplicationCode: "settlement", EnvironmentCode: "prod"}, event)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for key, expected := range map[string]string{"event_id": event.EventID, "application_code": "settlement", "environment_code": "prod", "actor_id": "user-1", "action": "SETTLEMENT_TEST", "resource_id": "inv-1"} {
		if got[key] != expected {
			t.Fatalf("%s=%v want %s", key, got[key], expected)
		}
	}
}

func TestValidateReceiptsRequiresEveryEvent(t *testing.T) {
	events := []Event{{EventID: "event-a"}, {EventID: "event-b"}}
	partial := bytes.NewBufferString(`{"code":"OK","data":[{"event_id":"event-a","status":"ACCEPTED"}]}`)
	if err := validateReceipts(partial, "PLATFORM_AUDIT", events); err == nil {
		t.Fatal("partial receipt unexpectedly accepted")
	}
	complete := bytes.NewBufferString(`{"code":"OK","data":[{"event_id":"event-a","status":"ACCEPTED"},{"event_id":"event-b","status":"DUPLICATE"}]}`)
	if err := validateReceipts(complete, "PLATFORM_AUDIT", events); err != nil {
		t.Fatal(err)
	}
}

func TestTaxCommandPayloadCarriesDeliveryIdentityAndSnapshot(t *testing.T) {
	event := Event{EventID: "ABCDEF0123456789ABCDEF0123456789", TenantID: "tenant-a", EventType: "SETTLEMENT_INVOICE_ISSUE_REQUESTED", CreatedAt: time.Date(2026, 9, 17, 1, 2, 3, 0, time.UTC), Payload: json.RawMessage(`{"attempt_id":"attempt-a","buyer_profile":{"name":"buyer"},"items":[{"item_name":"service"}]}`)}
	body, err := payloadFor(Destination{Name: "TAX_INVOICE_COMMAND"}, event)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["event_id"] != event.EventID || got["tenant_id"] != event.TenantID || got["event_type"] != event.EventType || got["attempt_id"] != "attempt-a" || got["buyer_profile"] == nil || got["items"] == nil {
		t.Fatalf("tax command envelope is incomplete: %v", got)
	}
}
