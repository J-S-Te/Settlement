package filegatewayclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDownloadUsesBearerAndBoundedContentRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/files/file-1/content" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token-1" {
			t.Fatalf("missing bearer token")
		}
		_, _ = w.Write([]byte("invoice"))
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client(), func(context.Context) (string, error) { return "token-1", nil })
	if err != nil {
		t.Fatal(err)
	}
	content, err := client.Download(context.Background(), "file-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "invoice" {
		t.Fatalf("unexpected content %q", content)
	}
}

func TestDownloadRejectsGatewayFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "hidden detail", http.StatusForbidden)
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client(), func(context.Context) (string, error) { return "token-1", nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Download(context.Background(), "file-1"); err == nil {
		t.Fatal("expected gateway failure")
	}
}
