package service

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/filegatewayclient"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestReceiptArchiveRecoveryHTTPRetiresBeforeExactRelease 验证回单网关HTTP边界及恢复约束。
// 参数：t 测试上下文。
// 返回值：无。
func TestReceiptArchiveRecoveryHTTPRetiresBeforeExactRelease(t *testing.T) {
	for _, state := range []string{"CANCELLED", "WRITING", "READY", "FAILED", "REJECTED"} {
		t.Run(state, func(t *testing.T) {
			retired, released := false, false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer machine" {
					t.Error("missing machine identity")
				}
				switch r.Method + " " + r.URL.Path {
				case "POST /api/v1/file-upload-requests/request/retire":
					retired = true
					file := "file"
					if state == "CANCELLED" {
						file = ""
					}
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"request_id": "request", "file_id": file, "status": state, "retired": true}})
				case "POST /api/v1/files/file/bindings/release":
					if !retired {
						t.Error("release preceded durable retirement")
					}
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					for k, v := range map[string]string{"application_id": "settlement", "resource_type": "receipt", "resource_id": "invoice", "binding_type": "receipt_image"} {
						if body[k] != v {
							t.Errorf("release %s=%q", k, body[k])
						}
					}
					released = true
				default:
					t.Errorf("unexpected recovery HTTP %s %s", r.Method, r.URL.Path)
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
			if err != nil {
				t.Fatal(err)
			}
			db := scriptdb.Open(t,
				scriptdb.Step{Kind: "begin"},
				scriptdb.Step{Kind: "query", SQL: "FOR UPDATE SKIP LOCKED", Rows: [][]driver.Value{{"effect", "invoice", "RECEIPT_IMAGE", "checksum", "invoice.pdf", "application/pdf", "settlement", server.URL, "request", "", "", "PENDING"}}},
				scriptdb.Step{Kind: "exec", SQL: "state='RETIRED'"}, scriptdb.Step{Kind: "commit"},
			)
			worker := &ReceiptArchiveWorker{DB: db, Identity: InvoiceArchiveIdentity{TenantID: "tenant", ApplicationID: "settlement", GatewayURL: server.URL}, Gateway: gateway}
			found, err := worker.recoverOne(context.Background())
			if err != nil || !found {
				t.Fatalf("recovery found=%v err=%v", found, err)
			}
			if !retired || released != (state != "CANCELLED") {
				t.Fatalf("retired=%v released=%v", retired, released)
			}
		})
	}
}

// TestReceiptArchiveRecoveryRejectsConfigurationDriftWithoutHTTP 验证回单网关HTTP边界及恢复约束。
// 参数：t 测试上下文。
// 返回值：无。
func TestReceiptArchiveRecoveryRejectsConfigurationDriftWithoutHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("drifted recovery reached external gateway")
		w.WriteHeader(500)
	}))
	defer server.Close()
	gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "FOR UPDATE SKIP LOCKED", Rows: [][]driver.Value{{"effect", "invoice", "RECEIPT_IMAGE", "checksum", "invoice.pdf", "application/pdf", "old-app", server.URL, "request", "", "", "PENDING"}}}, scriptdb.Step{Kind: "exec", SQL: "attempts=attempts+1"}, scriptdb.Step{Kind: "commit"})
	found, err := (&ReceiptArchiveWorker{DB: db, Identity: InvoiceArchiveIdentity{TenantID: "tenant", ApplicationID: "settlement", GatewayURL: server.URL}, Gateway: gateway}).recoverOne(context.Background())
	if err == nil || !found {
		t.Fatalf("drift was forgotten: found=%v err=%v", found, err)
	}
}

// TestReceiptArchiveRejectsUntrustedOrInvalidInputBeforePersistence 验证回单网关HTTP边界及恢复约束。
// 参数：t 测试上下文。
// 返回值：无。
func TestReceiptArchiveRejectsUntrustedOrInvalidInputBeforePersistence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid archive reached gateway")
		w.WriteHeader(500)
	}))
	defer server.Close()
	gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"permission", "tenant", "missing-tenant", "app", "origin", "checksum", "key", "type", "empty"} {
		t.Run(kind, func(t *testing.T) {
			p, identity, input := archivePrincipal("tenant"), InvoiceArchiveIdentity{TenantID: "tenant", ApplicationID: "app", GatewayURL: server.URL}, archiveInput("invoice", "key")
			input.DocumentType = "RECEIPT_IMAGE"
			switch kind {
			case "permission":
				p.Permissions = nil
			case "tenant":
				p.TenantID = "foreign"
			case "missing-tenant":
				identity.TenantID = ""
			case "app":
				identity.ApplicationID = ""
			case "origin":
				identity.GatewayURL = ""
			case "checksum":
				input.Checksum = "incorrect"
			case "key":
				input.Key = " "
			case "type":
				input.DocumentType = "unknown"
			case "empty":
				input.Content = nil
			}
			// An empty SQL script makes any accidental database access observable.
			db := scriptdb.Open(t)
			if _, _, err := (&Service{DB: db}).UploadReceiptDocument(context.Background(), p, identity, gateway, input); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}

// TestReceiptArchiveRecoveryHonorsLicenseBeforeDatabase 验证回单网关HTTP边界及恢复约束。
// 参数：t 测试上下文。
// 返回值：无。
func TestReceiptArchiveRecoveryHonorsLicenseBeforeDatabase(t *testing.T) {
	gateway, err := filegatewayclient.New("http://127.0.0.1:1", nil, func(context.Context) (string, error) {
		t.Error("denied recovery requested a machine token")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("license expired")
	worker := &ReceiptArchiveWorker{DB: scriptdb.Open(t), Identity: InvoiceArchiveIdentity{TenantID: "tenant", ApplicationID: "app", GatewayURL: "http://127.0.0.1:1"}, Gateway: gateway, CheckBusinessLicense: func(context.Context) error { return denied }}
	if err = worker.RunOnce(context.Background()); !errors.Is(err, denied) {
		t.Fatalf("license denial lost: %v", err)
	}
}

// TestReceiptArchiveRecoveryPersistsUnconfirmedGatewayFailure 验证回单网关HTTP边界及恢复约束。
// 参数：t 测试上下文。
// 返回值：无。
func TestReceiptArchiveRecoveryPersistsUnconfirmedGatewayFailure(t *testing.T) {
	for _, stage := range []string{"retire", "unconfirmed-retire", "release", "license-expired-after-retire"} {
		t.Run(stage, func(t *testing.T) {
			retired, released := false, false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/retire") {
					if stage == "retire" {
						w.WriteHeader(503)
						return
					}
					retired = true
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"request_id": "request", "file_id": "file", "status": "READY", "retired": stage != "unconfirmed-retire"}})
					return
				}
				released = true
				w.WriteHeader(503)
			}))
			defer server.Close()
			gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
			if err != nil {
				t.Fatal(err)
			}
			db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "FOR UPDATE SKIP LOCKED", Rows: [][]driver.Value{{"effect", "invoice", "RECEIPT_IMAGE", "checksum", "invoice.pdf", "application/pdf", "settlement", server.URL, "request", "", "", "PENDING"}}}, scriptdb.Step{Kind: "exec", SQL: "attempts=attempts+1"}, scriptdb.Step{Kind: "commit"})
			worker := &ReceiptArchiveWorker{DB: db, Identity: InvoiceArchiveIdentity{TenantID: "tenant", ApplicationID: "settlement", GatewayURL: server.URL}, Gateway: gateway}
			if stage == "license-expired-after-retire" {
				worker.CheckBusinessLicense = func(context.Context) error {
					if retired {
						return errors.New("license expired")
					}
					return nil
				}
			}
			found, err := worker.recoverOne(context.Background())
			if !found || err == nil {
				t.Fatalf("unconfirmed compensation disappeared: found=%v err=%v", found, err)
			}
			if released != (stage == "release") {
				t.Fatalf("release crossed unconfirmed retirement/license: retired=%v released=%v", retired, released)
			}
		})
	}
}
