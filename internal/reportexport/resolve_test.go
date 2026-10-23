package reportexport

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

func TestCompensationRetiresTerminalAndPendingUploadStatesWithoutReupload(t *testing.T) {
	cases := []struct {
		name, state, status       string
		missing, done, retireFail bool
	}{
		{"ready", "INTENT", "READY", false, true, false},
		{"failed", "INTENT", "FAILED", false, true, false},
		{"rejected", "INTENT", "REJECTED", false, true, false},
		{"writing-retired", "INTENT", "WRITING", false, true, false},
		{"sent-but-missing-retired", "INTENT", "", true, true, false},
		{"never-sent-retired", "PREPARED", "", true, true, false},
		{"writing-retirement-failed", "INTENT", "WRITING", false, false, true},
		{"missing-retirement-failed", "INTENT", "", true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := tc.state
			retried := false
			d := &exportDB{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(q, "SELECT status,lease_generation") {
					return &exportRows{columns: 4, values: [][]driver.Value{{"FAILED", int64(7), "", false}}}, nil
				}
				return &exportRows{columns: 6, values: [][]driver.Value{{"settlement", "original-request", "export.csv", []byte("retired secret content"), "", state}}}, nil
			}, exec: func(q string, _ []driver.NamedValue) (driver.Result, error) {
				if strings.Contains(q, "state='COMPENSATED'") {
					state = "COMPENSATED"
				}
				if strings.Contains(q, "attempts=attempts+1") {
					retried = true
				}
				return driver.RowsAffected(1), nil
			}}
			db := sql.OpenDB(d)
			defer db.Close()
			g := &recordingGateway{resolveStatus: tc.status, resolveMissing: tc.missing}
			if tc.retireFail {
				g.retireErr = errors.New("retirement unavailable")
			}
			w := &Worker{DB: db, TenantID: "tenant", Gateway: g, GatewayApplicationID: "settlement"}
			err := w.reconcileEffect(context.Background(), job{id: "job-1", tenantID: "tenant", generation: 7})
			if (err == nil) != tc.done {
				t.Fatalf("state=%s missing=%v done=%v error=%v", tc.status, tc.missing, tc.done, err)
			}
			if (state == "COMPENSATED") != tc.done || retried == tc.done {
				t.Fatalf("outcome lost: state=%s retried=%v", state, retried)
			}
			wantRelease := 0
			if tc.done && !tc.missing {
				wantRelease = 1
			}
			if g.uploads != 0 || g.resolves != 0 || g.releases != wantRelease || g.retires != 1 {
				t.Fatalf("invalid side effects: uploads=%d resolves=%d releases=%d retires=%d", g.uploads, g.resolves, g.releases, g.retires)
			}
		})
	}
}

func TestGenerateCommitsSentBoundaryBeforeUpload(t *testing.T) {
	prepared, intent := false, false
	d := &exportDB{exec: func(q string, _ []driver.NamedValue) (driver.Result, error) {
		if strings.Contains(q, "INSERT INTO settlement_report_export_effect") && strings.Contains(q, "'PREPARED'") {
			prepared = true
		}
		if strings.Contains(q, "SET state='INTENT'") {
			if !prepared {
				t.Fatal("sent marker preceded durable preparation")
			}
			intent = true
		}
		return driver.RowsAffected(1), nil
	}}
	db := sql.OpenDB(d)
	defer db.Close()
	g := &recordingGateway{beforeUpload: func() {
		if d.commits != 2 || !prepared || !intent {
			t.Fatalf("upload started before separate durable preparation/intent commits: commits=%d prepared=%v intent=%v", d.commits, prepared, intent)
		}
	}}
	w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker", Gateway: g, GatewayMode: "required", GatewayApplicationID: "settlement"}
	if err := w.generate(context.Background(), job{id: "job-1", tenantID: "tenant", generation: 7}); err != nil {
		t.Fatal(err)
	}
	if !prepared || !intent || g.uploads != 1 {
		t.Fatalf("missing durable sent boundary: prepared=%v intent=%v uploads=%d", prepared, intent, g.uploads)
	}
}
