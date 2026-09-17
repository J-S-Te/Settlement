package creditclient

import "testing"

func TestNewRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := New("http://crm", "http://platform/token", "client", "", "scope", nil); err == nil {
		t.Fatal("expected incomplete OAuth configuration to fail")
	}
}

func TestNewAcceptsConfiguredEndpoint(t *testing.T) {
	if _, err := New("http://crm/api/v1/internal/credit/payment-events", "http://platform/token", "client", "secret", "scope", nil); err != nil {
		t.Fatalf("expected configured client: %v", err)
	}
}
