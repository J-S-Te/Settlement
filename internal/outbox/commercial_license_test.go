package outbox

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	core "github.com/J-S-Te/license-core"
	licruntime "github.com/J-S-Te/license-core/runtime"
)

func signedOutboxBusinessCheck(t *testing.T) (func(context.Context) error, func(time.Time)) {
	t.Helper()
	pp, ps, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vp, vs, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	binding := licruntime.Binding{InstanceID: "instance", Environment: "prod", Application: "settlement", ServiceID: "settlement-worker"}
	l := core.License{ProtocolVersion: core.ProtocolVersion, Issuer: core.Issuer, ID: "test-license", Version: 1, CustomerID: "customer", ProductID: core.Product, Environment: binding.Environment, InstanceID: binding.InstanceID, IssuedAt: now.Unix() - 100, NotBefore: now.Unix() - 100, Applications: []core.Application{{Code: "settlement", Kind: "FULL", NotBefore: now.Unix() - 100, ExpiresAt: now.Unix() + 10}}}
	raw, err := core.Sign(l, "vendor", vs)
	if err != nil {
		t.Fatal(err)
	}
	s := licruntime.PlatformSnapshot{Protocol: licruntime.Protocol, InstanceID: binding.InstanceID, Environment: binding.Environment, Application: binding.Application, ServiceID: binding.ServiceID, Revision: 1, EnforcementState: licruntime.Pending, MigrationEligible: true, CurrentLicenseJWS: raw, HighestLicenseVersion: 1, SnapshotIssuedAt: now.Unix()}
	snapshot, err := licruntime.SignSnapshot(s, "platform", ps)
	if err != nil {
		t.Fatal(err)
	}
	r, err := licruntime.NewFileRuntime(filepath.Join(t.TempDir(), "state.json"), binding, map[string]ed25519.PublicKey{"platform": pp}, map[string]ed25519.PublicKey{"vendor": vp})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.ApplySnapshot(context.Background(), snapshot, now); err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context) error { return r.Evaluate(ctx, core.MUTATE_BUSINESS, now) }, func(at time.Time) { now = at }
}

func TestExpiredTaxDeliveryDoesNotAcquireOrConsumeRetry(t *testing.T) {
	check, advance := signedOutboxBusinessCheck(t)
	advance(time.Unix(1800000010, 0))
	w := &Worker{Destinations: []Destination{{Name: "TAX_INVOICE_COMMAND"}}, CheckBusinessLicense: check}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.deliver(context.Background(), Destination{Name: "TAX_INVOICE_COMMAND"}, nil); !errors.Is(err, ErrLicensePaused) {
		t.Fatal(err)
	}
}

func TestSignedExpiryAfterTaxTokenBlocksOutboundBusinessRequest(t *testing.T) {
	check, advance := signedOutboxBusinessCheck(t)
	advance(time.Unix(1800000009, 0))
	tokens, deliveries := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens++
			advance(time.Unix(1800000010, 0))
			fmt.Fprint(w, `{"access_token":"test-token","token_type":"Bearer","expires_in":300,"scope":"tax.write"}`)
			return
		}
		deliveries++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	w := &Worker{CheckBusinessLicense: check, HTTP: server.Client(), TokenEndpoint: server.URL + "/token", tokens: map[string]cachedToken{}}
	d := Destination{Name: "TAX_INVOICE_COMMAND", Scope: "tax.write", Endpoint: server.URL + "/business"}
	events := []Event{{EventID: "event", TenantID: "tenant", EventType: "tax.issue", Payload: json.RawMessage(`{"attempt_id":"attempt"}`)}}
	if err := w.deliver(context.Background(), d, events); !errors.Is(err, ErrLicensePaused) {
		t.Fatalf("expiry before HTTP side effect did not pause: %v", err)
	}
	if tokens != 1 || deliveries != 0 {
		t.Fatalf("token requests=%d business deliveries=%d", tokens, deliveries)
	}
}

func TestSignedExpiryKeepsExistingAuditAndNotificationDelivery(t *testing.T) {
	for _, name := range []string{"PLATFORM_AUDIT", "PLATFORM_NOTIFICATION"} {
		t.Run(name, func(t *testing.T) {
			check, advance := signedOutboxBusinessCheck(t)
			advance(time.Unix(1800000010, 0))
			tokens, deliveries := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokens++
					fmt.Fprint(w, `{"access_token":"test-token","token_type":"Bearer","expires_in":300,"scope":"essential.write"}`)
					return
				}
				deliveries++
				var body struct {
					Events []json.RawMessage `json:"events"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) != 1 {
					t.Errorf("missing delivery event: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if name == "PLATFORM_NOTIFICATION" {
					fmt.Fprint(w, `{"code":"OK","data":{"receipts":[{"event_id":"event","status":"ACCEPTED"}]}}`)
				} else {
					fmt.Fprint(w, `{"code":"OK","data":[{"event_id":"event","status":"ACCEPTED"}]}`)
				}
			}))
			defer server.Close()
			w := &Worker{CheckBusinessLicense: check, HTTP: server.Client(), TokenEndpoint: server.URL + "/token", tokens: map[string]cachedToken{}}
			d := Destination{Name: name, Scope: "essential.write", Endpoint: server.URL + "/essential", ApplicationCode: "settlement", EnvironmentCode: "prod"}
			events := []Event{{EventID: "event", TenantID: "tenant", EventType: "existing-event", Payload: json.RawMessage(`{"actor_id":"user"}`)}}
			if err := w.deliver(context.Background(), d, events); err != nil {
				t.Fatalf("existing essential delivery denied: %v", err)
			}
			if tokens != 1 || deliveries != 1 {
				t.Fatalf("token requests=%d essential deliveries=%d", tokens, deliveries)
			}
		})
	}
}
