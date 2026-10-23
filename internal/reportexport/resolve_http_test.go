package reportexport

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/filegatewayclient"
)

func TestCompensationUsesRealHTTPRetireAndBindingReleaseWithoutResolve(t *testing.T) {
	for _, status := range []string{"MISSING", "MISSING-THEN-WRITING", "WRITING", "READY", "FAILED", "REJECTED", "DELETED"} {
		t.Run(status, func(t *testing.T) {
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				if r.Header.Get("Authorization") != "Bearer test-machine" {
					t.Error("machine authorization missing")
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/file-upload-requests/retired-request":
					if status == "DELETED" {
						w.WriteHeader(http.StatusConflict)
						return
					}
					if strings.HasPrefix(status, "MISSING") {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"request_id": "retired-request", "file_id": "retired-file", "status": status}})
				case "POST /api/v1/file-upload-requests/retired-request/retire":
					fileID, retiredStatus := "retired-file", status
					if status == "MISSING" {
						fileID, retiredStatus = "", "CANCELLED"
					}
					if status == "MISSING-THEN-WRITING" {
						retiredStatus = "WRITING"
					}
					if status == "DELETED" {
						retiredStatus = "READY"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"request_id": "retired-request", "file_id": fileID, "status": retiredStatus, "retired": true}})
				case "POST /api/v1/files/retired-file/bindings/release":
					var payload map[string]string
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					for key, value := range map[string]string{"application_id": "settlement", "resource_type": "settlement_report_export", "resource_id": "job-1", "binding_type": "REPORT"} {
						if payload[key] != value {
							t.Errorf("invalid release %s=%q", key, payload[key])
						}
					}
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected compensation request; content must not be uploaded: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "test-machine", nil })
			if err != nil {
				t.Fatal(err)
			}
			done := false
			d := &exportDB{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(q, "SELECT status,lease_generation") {
					return &exportRows{columns: 4, values: [][]driver.Value{{"FAILED", int64(7), "", false}}}, nil
				}
				return &exportRows{columns: 6, values: [][]driver.Value{{"settlement", "retired-request", "export.csv", []byte("never retransmit this report"), "", "INTENT"}}}, nil
			}, exec: func(q string, _ []driver.NamedValue) (driver.Result, error) {
				if strings.Contains(q, "state='COMPENSATED'") {
					done = true
				}
				return driver.RowsAffected(1), nil
			}}
			db := sql.OpenDB(d)
			defer db.Close()
			worker := &Worker{DB: db, TenantID: "tenant", Gateway: gateway, GatewayApplicationID: "settlement"}
			if err := worker.reconcileEffect(context.Background(), job{id: "job-1", tenantID: "tenant", generation: 7}); err != nil {
				t.Fatal(err)
			}
			want := []string{"POST /api/v1/file-upload-requests/retired-request/retire"}
			if status != "MISSING" {
				want = append(want, "POST /api/v1/files/retired-file/bindings/release")
			}
			if !done || !reflect.DeepEqual(requests, want) {
				t.Fatalf("compensation not confirmed: done=%v requests=%v", done, requests)
			}
		})
	}
}
