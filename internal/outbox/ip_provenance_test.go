package outbox

import (
	"encoding/json"
	"testing"
	"time"
)

// AUD-IP-2026：业务事务内记录的登录地址、操作地址与来源标记必须提升为平台协议字段；
// 否则平台只能拿投递连接地址冒充用户地址。
func TestAuditPayloadCarriesLoginClientAndSourceIP(t *testing.T) {
	destination := Destination{Name: "PLATFORM_AUDIT", ApplicationCode: "settlement", EnvironmentCode: "dev"}
	event := Event{
		EventID: "event-1", EventType: "SETTLEMENT_INVOICE_REQUEST_SUBMITTED",
		AggregateType: "invoice_request", AggregateID: "request-1", CreatedAt: time.Now().UTC(),
		Payload: json.RawMessage(`{"actor_id":"user-1","result":"SUCCESS","login_ip":"203.0.113.10","client_ip":"10.8.0.5","ip_source":"TRUSTED_PROXY"}`),
	}

	body, err := payloadFor(destination, event)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for key, want := range map[string]string{
		"login_ip": "203.0.113.10", "client_ip": "10.8.0.5", "ip_source": "TRUSTED_PROXY",
	} {
		if got, _ := decoded[key].(string); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestAuditPayloadOmitsMissingIPFields(t *testing.T) {
	destination := Destination{Name: "PLATFORM_AUDIT", ApplicationCode: "settlement", EnvironmentCode: "dev"}
	event := Event{EventID: "event-2", EventType: "SETTLEMENT_INVOICE_REQUEST_SUBMITTED", AggregateType: "invoice_request", CreatedAt: time.Now().UTC(), Payload: json.RawMessage(`{"actor_id":"user-1"}`)}

	body, err := payloadFor(destination, event)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	for _, key := range []string{"login_ip", "client_ip", "ip_source"} {
		if _, found := decoded[key]; found {
			t.Errorf("%s 不应凭空出现", key)
		}
	}
}
