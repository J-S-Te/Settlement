package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFileGatewayTokenUsesBasicWithoutFormCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != "qa-client" || secret != "qa-secret" {
			t.Error("machine Basic authentication absent")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Has("client_id") || r.Form.Has("client_secret") || r.Form.Get("scope") != "file.write" || r.Form.Get("grant_type") != "client_credentials" {
			t.Error("invalid machine token form")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"qa-token"}`))
	}))
	defer server.Close()
	token, err := requestFileGatewayToken(context.Background(), server.Client(), server.URL, "qa-client", "qa-secret", "file.write")
	if err != nil || token != "qa-token" {
		t.Fatalf("token request failed: %v", err)
	}
}

func TestFileGatewayTokenRejectsErrorsWithoutResponseDisclosure(t *testing.T) {
	for _, body := range []string{`{"access_token":""}`, `not-json`, `{"error":"qa-sensitive-secret"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, err := requestFileGatewayToken(context.Background(), server.Client(), server.URL, "qa-client", "qa-secret", "file.write")
		server.Close()
		if err == nil || strings.Contains(err.Error(), "qa-sensitive-secret") {
			t.Fatal("invalid response accepted or disclosed")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte("qa-sensitive-secret"))
	}))
	defer server.Close()
	if _, err := requestFileGatewayToken(context.Background(), server.Client(), server.URL, "qa-client", "qa-secret", "file.write"); err == nil || strings.Contains(err.Error(), "qa-sensitive-secret") {
		t.Fatal("HTTP failure accepted or disclosed")
	}
}
