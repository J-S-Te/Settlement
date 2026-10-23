package creditclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := New("http://crm", "http://platform/token", "client", "", "scope", nil); err == nil {
		t.Fatal("expected incomplete OAuth configuration to fail")
	}
}

func TestPublishLicenseExpiryDuringOAuthPreventsBusinessPOST(t *testing.T) {
	var expired atomic.Bool
	var tokens, posts atomic.Int32
	denied := errors.New("expired while obtaining OAuth token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens.Add(1)
			expired.Store(true)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"credit-test-token","token_type":"Bearer","expires_in":300}`)
			return
		}
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := New(server.URL+"/events", server.URL+"/token", "client", "secret", "credit.write", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.SetBusinessLicenseCheck(func(context.Context) error {
		if expired.Load() {
			return denied
		}
		return nil
	})
	err = client.Publish(context.Background(), "tenant", "event", []byte(`{}`))
	if !errors.Is(err, denied) || tokens.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("token-time expiry leaked business POST: tokens=%d posts=%d error=%v", tokens.Load(), posts.Load(), err)
	}
}

func TestPublishOAuthRemainsCompatibleWithNilOrValidGate(t *testing.T) {
	for _, gated := range []bool{false, true} {
		t.Run(fmt.Sprintf("gated=%v", gated), func(t *testing.T) {
			var tokens, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokens.Add(1)
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"access_token":"credit-test-token","token_type":"Bearer","expires_in":300}`)
					return
				}
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer credit-test-token" || r.Header.Get("X-Tenant-ID") != "tenant" || r.Header.Get("X-Integration-Nonce") == "" {
					t.Error("business request lost OAuth or integration metadata")
				}
				posts.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			client, err := New(server.URL+"/events", server.URL+"/token", "client", "secret", "credit.write", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if gated {
				client.SetBusinessLicenseCheck(func(context.Context) error { return nil })
			}
			for i := 0; i < 2; i++ {
				if err := client.Publish(context.Background(), "tenant", "event", []byte(`{}`)); err != nil {
					t.Fatal(err)
				}
			}
			if tokens.Load() != 1 || posts.Load() != 2 {
				t.Fatalf("token reuse/delivery changed: tokens=%d posts=%d", tokens.Load(), posts.Load())
			}
		})
	}
}

func TestNewAcceptsConfiguredEndpoint(t *testing.T) {
	if _, err := New("http://crm/api/v1/internal/credit/payment-events", "http://platform/token", "client", "secret", "scope", nil); err != nil {
		t.Fatalf("expected configured client: %v", err)
	}
}

func TestPublishSetsIntegrationTimestampAndNonce(t *testing.T) {
	var gotTimestamp, gotNonce string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTimestamp = r.Header.Get("X-Integration-Timestamp")
		gotNonce = r.Header.Get("X-Integration-Nonce")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := &Client{endpoint: server.URL, client: server.Client()}
	if err := client.Publish(context.Background(), "tenant-a", "event-1", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339Nano, gotTimestamp); err != nil || len(gotNonce) < 32 {
		t.Fatalf("timestamp=%q nonce=%q", gotTimestamp, gotNonce)
	}
}
