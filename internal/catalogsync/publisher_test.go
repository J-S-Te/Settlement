package catalogsync

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublishUsesDedicatedPublisherAndPublishesManifest(t *testing.T) {
	const manifest = `{"catalog_version":"2","roles":[{"code":"settlement_admin"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			clientID, clientSecret, ok := r.BasicAuth()
			if !ok || clientID != "settlement-prod-catalog-publisher" || clientSecret != "publisher-secret" {
				t.Fatal("token request did not use the dedicated publisher credential")
			}
			if err := r.ParseForm(); err != nil || r.Form.Get("scope") != "authorization.catalog.sync" {
				t.Fatal("token request scope is invalid")
			}
			_, _ = io.WriteString(w, `{"access_token":"catalog-token"}`)
		case "/api/v1/applications/application-1/authorization-catalog":
			if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer catalog-token" {
				t.Fatal("catalog request authorization is invalid")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != manifest {
				t.Fatalf("catalog body = %s", body)
			}
			_, _ = io.WriteString(w, `{"data":{"sync_status":"SYNCED"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := Publish(context.Background(), server.Client(), Config{
		PlatformBaseURL: server.URL,
		ApplicationID:   "application-1",
		ClientID:        "settlement-prod-catalog-publisher",
		ClientSecret:    "publisher-secret",
	}, []byte(manifest))
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
}

func TestPublishDoesNotExposeSecretInErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "publisher-secret", http.StatusUnauthorized)
	}))
	defer server.Close()

	err := Publish(context.Background(), server.Client(), Config{
		PlatformBaseURL: server.URL,
		ApplicationID:   "application-1",
		ClientID:        "publisher",
		ClientSecret:    "publisher-secret",
	}, []byte(`{}`))
	if err == nil || strings.Contains(err.Error(), "publisher-secret") {
		t.Fatalf("Publish() error = %v", err)
	}
}
