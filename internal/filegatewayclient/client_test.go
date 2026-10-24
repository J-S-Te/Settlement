package filegatewayclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type resourceReleaser interface {
	Release(context.Context, string, string, string, string, string) error
}

type uploadResolver interface {
	ResolveUpload(context.Context, string) (string, string, bool, error)
}

type uploadRetirer interface {
	RetireUpload(context.Context, string) (string, string, error)
}

func TestRetireUploadConfirmsPersistentRequestTombstone(t *testing.T) {
	for _, status := range []string{"CANCELLED", "WRITING", "READY", "FAILED", "REJECTED"} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/file-upload-requests/request-1/retire" || r.URL.RawQuery != "" {
					t.Errorf("invalid retirement route: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer token-1" {
					t.Error("retirement bearer missing")
				}
				body, _ := io.ReadAll(r.Body)
				if len(body) != 0 {
					t.Error("retirement retransmitted content or supplied client scope")
				}
				fileID := "file-1"
				if status == "CANCELLED" {
					fileID = ""
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"request_id": "request-1", "file_id": fileID, "status": status, "retired": true}})
			}))
			defer server.Close()
			client, _ := New(server.URL, server.Client(), func(context.Context) (string, error) { return "token-1", nil })
			retirer, ok := any(client).(uploadRetirer)
			if !ok {
				t.Fatal("gateway client cannot persistently retire an upload request")
			}
			fileID, gotStatus, err := retirer.RetireUpload(context.Background(), "request-1")
			if err != nil {
				t.Fatal(err)
			}
			if gotStatus != status || (status == "CANCELLED") != (fileID == "") {
				t.Fatalf("retire=%q,%q", fileID, gotStatus)
			}
		})
	}
}

func TestRetireUploadRejectsUnconfirmedRetirement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"request_id":"request-1","file_id":"file-1","status":"READY","retired":false}}`)
	}))
	defer server.Close()
	client, _ := New(server.URL, server.Client(), func(context.Context) (string, error) { return "token-1", nil })
	retirer, ok := any(client).(uploadRetirer)
	if !ok {
		t.Fatal("retirer missing")
	}
	if _, _, err := retirer.RetireUpload(context.Background(), "request-1"); err == nil {
		t.Fatal("unconfirmed retirement allowed cleanup completion")
	}
}

func TestResolveUploadUsesReadOnlyTenantAndApplicationBoundRequestLookup(t *testing.T) {
	for _, status := range []string{"WRITING", "READY", "FAILED", "REJECTED", "MISSING"} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/file-upload-requests/request-1" || r.URL.RawQuery != "" {
					t.Errorf("invalid resolve request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer token-1" {
					t.Error("resolve bearer missing")
				}
				body, _ := io.ReadAll(r.Body)
				if len(body) != 0 {
					t.Error("resolve retransmitted file content")
				}
				if status == "MISSING" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"request_id": "request-1", "file_id": "file-1", "status": status}})
			}))
			defer server.Close()
			client, err := New(server.URL, server.Client(), func(context.Context) (string, error) { return "token-1", nil })
			if err != nil {
				t.Fatal(err)
			}
			resolver, ok := any(client).(uploadResolver)
			if !ok {
				t.Fatal("gateway client cannot resolve an upload request without retransmitting bytes")
			}
			fileID, gotStatus, found, err := resolver.ResolveUpload(context.Background(), "request-1")
			if err != nil {
				t.Fatal(err)
			}
			if status == "MISSING" {
				if found || fileID != "" {
					t.Fatal("404 was interpreted as an existing upload")
				}
				return
			}
			if !found || fileID != "file-1" || gotStatus != status {
				t.Fatalf("resolve=%q,%q,%v", fileID, gotStatus, found)
			}
		})
	}
}

func TestResolveUploadRejectsWrongIdentityAndUnknownState(t *testing.T) {
	for _, payload := range []string{`{"data":{"request_id":"other","file_id":"file-1","status":"READY"}}`, `{"data":{"request_id":"request-1","file_id":"file-1","status":"DELETING"}}`, `{"data":{"request_id":"request-1","file_id":"","status":"FAILED"}}`} {
		t.Run(payload, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, payload) }))
			defer server.Close()
			client, _ := New(server.URL, server.Client(), func(context.Context) (string, error) { return "token-1", nil })
			resolver, ok := any(client).(uploadResolver)
			if !ok {
				t.Fatal("resolver missing")
			}
			if _, _, _, err := resolver.ResolveUpload(context.Background(), "request-1"); err == nil {
				t.Fatal("malformed request lookup accepted")
			}
		})
	}
}

func TestReleaseUsesScopedIdempotentGatewayResourceContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/files/file-1/bindings/release" {
			t.Errorf("unexpected release route: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token-1" {
			t.Error("missing release bearer")
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"application_id": "settlement", "resource_type": "settlement_report_export", "resource_id": "job-1", "binding_type": "REPORT"} {
			if payload[key] != want {
				t.Errorf("release %s=%q, want %q", key, payload[key], want)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client(), func(context.Context) (string, error) { return "token-1", nil })
	if err != nil {
		t.Fatal(err)
	}
	releaser, ok := any(client).(resourceReleaser)
	if !ok {
		t.Fatal("gateway client cannot compensate a resource binding after an unknown Bind response")
	}
	for range 2 {
		if err = releaser.Release(context.Background(), "settlement", "file-1", "settlement_report_export", "job-1", "REPORT"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUploadPreservesCSVMediaTypeAndExportMetadata(t *testing.T) {
	const filename = "结算报表 \"十月\".csv"
	const content = "项目,金额\n测试,100\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/files" {
			t.Errorf("unexpected upload route: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token-1" || r.Header.Get("Idempotency-Key") != "request-1" {
			t.Error("missing upload authorization or idempotency key")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse upload multipart: %v", err)
			http.Error(w, "invalid multipart", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if got := r.FormValue("application_id"); got != "settlement" {
			t.Errorf("application_id = %q", got)
		}
		if got := r.FormValue("classification"); got != "INTERNAL" {
			t.Errorf("classification = %q, want INTERNAL", got)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("read file part: %v", err)
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		defer file.Close()
		if header.Filename != filename {
			t.Errorf("filename = %q, want %q", header.Filename, filename)
		}
		if got := header.Header.Get("Content-Type"); got != "text/csv; charset=utf-8" {
			t.Errorf("file Content-Type = %q, want text/csv; charset=utf-8", got)
		}
		got, err := io.ReadAll(file)
		if err != nil || string(got) != content {
			t.Errorf("file content = %q, error = %v", got, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"file_id":"export-file-1"}}`)
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client(), func(context.Context) (string, error) { return "token-1", nil })
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := client.Upload(context.Background(), "request-1", "settlement", "REPORT_EXPORT", filename, "text/csv; charset=utf-8", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if fileID != "export-file-1" {
		t.Fatalf("file ID = %q", fileID)
	}
}

func TestUploadRejectsInvalidMediaTypeBeforeRequest(t *testing.T) {
	for _, mediaType := range []string{"text/csv; charset", "text/csv; charset=\"unterminated", "text/csv\r\nX-Injected: true"} {
		t.Run(mediaType, func(t *testing.T) {
			tokenCalled := false
			client, err := New("http://127.0.0.1:1", nil, func(context.Context) (string, error) {
				tokenCalled = true
				return "token-1", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			fileID, err := client.Upload(context.Background(), "request-1", "settlement", "REPORT_EXPORT", "export.csv", mediaType, strings.NewReader("amount\n100\n"))
			if err == nil || err.Error() != "invalid upload media type" {
				t.Fatalf("expected invalid media type error, got %v", err)
			}
			if fileID != "" || tokenCalled {
				t.Fatalf("invalid media type reached request stage: file ID = %q, token called = %v", fileID, tokenCalled)
			}
		})
	}
}

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
