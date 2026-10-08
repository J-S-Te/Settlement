package filegatewayclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
