package reportexport

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingGateway struct {
	uploads, binds, releases         int
	releaseErr                       error
	uploadErr, bindErr               error
	requestID, filename, content     string
	requestIDs, fileIDs, releasedIDs []string
	resolves                         int
	resolveFileID, resolveStatus     string
	resolveMissing                   bool
	resolveErr                       error
	beforeUpload                     func()
	retires                          int
	retireFileID, retireStatus       string
	retireErr                        error
}

func (g *recordingGateway) RetireUpload(_ context.Context, requestID string) (string, string, error) {
	g.retires++
	g.requestID = requestID
	if g.retireErr != nil {
		return "", "", g.retireErr
	}
	if g.resolveMissing && g.retireFileID == "" {
		return "", "CANCELLED", nil
	}
	fileID, status := g.retireFileID, g.retireStatus
	if fileID == "" {
		fileID = g.resolveFileID
	}
	if fileID == "" {
		fileID = "file-1"
	}
	if status == "" {
		status = g.resolveStatus
	}
	if status == "" {
		status = "READY"
	}
	return fileID, status, nil
}

func (g *recordingGateway) ResolveUpload(_ context.Context, requestID string) (string, string, bool, error) {
	g.resolves++
	g.requestID = requestID
	if g.resolveErr != nil {
		return "", "", false, g.resolveErr
	}
	if g.resolveMissing {
		return "", "", false, nil
	}
	fileID, status := g.resolveFileID, g.resolveStatus
	if fileID == "" {
		fileID = "file-1"
	}
	if status == "" {
		status = "READY"
	}
	return fileID, status, true, nil
}

func (g *recordingGateway) Upload(_ context.Context, requestID, _, _, filename, _ string, content io.Reader) (string, error) {
	if g.beforeUpload != nil {
		g.beforeUpload()
	}
	g.uploads++
	g.requestID, g.filename = requestID, filename
	g.requestIDs = append(g.requestIDs, requestID)
	data, err := io.ReadAll(content)
	if err != nil {
		return "", err
	}
	g.content = string(data)
	if g.uploadErr != nil {
		return "", g.uploadErr
	}
	if len(g.fileIDs) >= g.uploads {
		return g.fileIDs[g.uploads-1], nil
	}
	return "file-1", nil
}
func (g *recordingGateway) Bind(context.Context, string, string, string, string, string, string) error {
	g.binds++
	return g.bindErr
}
func (g *recordingGateway) Release(_ context.Context, _ string, fileID string, _, _, _ string) error {
	g.releases++
	g.releasedIDs = append(g.releasedIDs, fileID)
	return g.releaseErr
}

func TestCompensationOfOldGenerationCannotReleaseNewGenerationFile(t *testing.T) {
	d := &exportDB{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
		if strings.Contains(q, "SELECT status,lease_generation") {
			return &exportRows{columns: 4, values: [][]driver.Value{{"READY", int64(8), "file-new", false}}}, nil
		}
		if strings.Contains(q, "SELECT application_id,request_id") {
			return &exportRows{columns: 6, values: [][]driver.Value{{"settlement", "old-request", "export.csv", []byte("same CSV"), "file-old", "INTENT"}}}, nil
		}
		return &exportRows{columns: 9}, nil
	}}
	db := sql.OpenDB(d)
	defer db.Close()
	g := &recordingGateway{fileIDs: []string{"file-old", "file-new"}, retireFileID: "file-old"}
	w := &Worker{DB: db, TenantID: "tenant", Gateway: g, GatewayApplicationID: "settlement", WorkerID: "worker", GatewayMode: "required"}
	for _, generation := range []uint64{7, 8} {
		if err := w.generate(context.Background(), job{id: "job1", tenantID: "tenant", reportType: InvoicedReceivablesCSV, generation: generation}); err != nil {
			t.Fatal(err)
		}
	}
	if len(g.requestIDs) != 2 || g.requestIDs[0] == g.requestIDs[1] {
		t.Fatalf("generations shared upload identity: %v", g.requestIDs)
	}
	if !strings.Contains(g.requestIDs[0], "job1-7-") || !strings.Contains(g.requestIDs[1], "job1-8-") {
		t.Fatalf("generation not in upload identity: %v", g.requestIDs)
	}
	if err := w.reconcileEffect(context.Background(), job{id: "job1", tenantID: "tenant", generation: 7}); err != nil {
		t.Fatal(err)
	}
	if len(g.releasedIDs) != 1 || g.releasedIDs[0] != "file-old" {
		t.Fatalf("old compensation touched current file: %v", g.releasedIDs)
	}
}

func TestExpiredLeaseDoesNotUploadOrBind(t *testing.T) {
	d := &exportDB{exec: func(string, []driver.NamedValue) (driver.Result, error) { return driver.RowsAffected(0), nil }}
	db := sql.OpenDB(d)
	defer db.Close()
	g := &recordingGateway{}
	w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker", Gateway: g, GatewayMode: "required", GatewayApplicationID: "settlement"}
	w.generate(context.Background(), job{id: "job-1", tenantID: "tenant", reportType: InvoicedReceivablesCSV})
	if g.uploads != 0 || g.binds != 0 {
		t.Fatalf("expired lease performed gateway side effects: uploads=%d binds=%d", g.uploads, g.binds)
	}
}

// 驱动隔离外部 SQL，同时执行真实 CSV 及发布路径。
type exportDB struct {
	mu      sync.Mutex
	queries []string
	execs   []string
	args    [][]driver.NamedValue
	commits int
	exec    func(string, []driver.NamedValue) (driver.Result, error)
	query   func(string, []driver.NamedValue) (driver.Rows, error)
}

func (d *exportDB) Connect(context.Context) (driver.Conn, error) { return &exportConn{d}, nil }
func (d *exportDB) Driver() driver.Driver                        { return exportDriver{} }

type exportDriver struct{}

func (exportDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type exportConn struct{ d *exportDB }

func (c *exportConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *exportConn) Close() error              { return nil }
func (c *exportConn) Begin() (driver.Tx, error) { return exportTx{d: c.d}, nil }
func (c *exportConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.execs = append(c.d.execs, q)
	c.d.args = append(c.d.args, append([]driver.NamedValue(nil), args...))
	if c.d.exec != nil {
		return c.d.exec(q, args)
	}
	return driver.RowsAffected(1), nil
}
func (c *exportConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.queries = append(c.d.queries, q)
	if c.d.query != nil {
		return c.d.query(q, args)
	}
	return &exportRows{columns: 9}, nil
}

type exportTx struct{ d *exportDB }

func (tx exportTx) Commit() error { tx.d.mu.Lock(); defer tx.d.mu.Unlock(); tx.d.commits++; return nil }
func (exportTx) Rollback() error  { return nil }

type exportRows struct {
	columns int
	values  [][]driver.Value
}

func (r *exportRows) Columns() []string { return make([]string, r.columns) }
func (r *exportRows) Close() error      { return nil }
func (r *exportRows) Next(dst []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dst, r.values[0])
	r.values = r.values[1:]
	return nil
}

func TestGenerateFilenameIsStableForIdenticalRetry(t *testing.T) {
	d := &exportDB{}
	db := sql.OpenDB(d)
	defer db.Close()
	w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker"}
	w.generate(context.Background(), job{id: "job-1", tenantID: "tenant", reportType: InvoicedReceivablesCSV})
	var filename string
	for i, q := range d.execs {
		if strings.Contains(q, "SET status='READY'") {
			filename = d.args[i][0].Value.(string)
		}
	}
	if !strings.HasPrefix(filename, "invoiced-receivables-job-1-") {
		t.Fatalf("filename %q must derive from stable job identity and content, not wall clock", filename)
	}
}

func TestZeroRowPublicationLeavesRecoverableIntent(t *testing.T) {
	d := &exportDB{exec: func(q string, _ []driver.NamedValue) (driver.Result, error) {
		if strings.Contains(q, "SET status='READY'") {
			return driver.RowsAffected(0), nil
		}
		return driver.RowsAffected(1), nil
	}}
	db := sql.OpenDB(d)
	defer db.Close()
	g := &recordingGateway{}
	w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker", Gateway: g, GatewayMode: "required", GatewayApplicationID: "settlement"}
	err := w.generate(context.Background(), job{id: "job-1", tenantID: "tenant", reportType: InvoicedReceivablesCSV, generation: 7})
	if !errors.Is(err, errLeaseLost) {
		t.Fatalf("zero-row publication must report lost lease, got %v", err)
	}
	intent := false
	for _, q := range d.execs {
		if strings.Contains(q, "INSERT INTO settlement_report_export_effect") {
			intent = true
		}
		if strings.Contains(q, "state='PUBLISHED'") {
			t.Fatal("unpublished file was marked published")
		}
		if strings.Contains(q, "status='FAILED'") {
			t.Fatal("lost lease overwrote job status")
		}
	}
	if !intent || g.binds != 1 {
		t.Fatalf("lost publication must preserve durable gateway intent; intent=%v binds=%d", intent, g.binds)
	}
}

func TestHeartbeatCancelsExecutionWhenRenewalLosesLease(t *testing.T) {
	d := &exportDB{exec: func(string, []driver.NamedValue) (driver.Result, error) { return driver.RowsAffected(0), nil }}
	db := sql.OpenDB(d)
	defer db.Close()
	w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker", LeaseDuration: time.Second, HeartbeatInterval: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := w.heartbeat(ctx, job{id: "job-1", tenantID: "tenant"}, cancel)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("failed renewal did not cancel generation")
	}
}

func TestReconciliationRecoversUnknownUploadAndPersistsFailedReleaseForRetry(t *testing.T) {
	storedID := ""
	state := "INTENT"
	retry := false
	d := &exportDB{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
		if strings.Contains(q, "SELECT status,lease_generation") {
			return &exportRows{columns: 4, values: [][]driver.Value{{"PROCESSING", int64(8), "", true}}}, nil
		}
		return &exportRows{columns: 6, values: [][]driver.Value{{"settlement", "request-original", "invoiced-receivables-job-1-hash.csv", []byte("exact original csv"), storedID, state}}}, nil
	}, exec: func(q string, args []driver.NamedValue) (driver.Result, error) {
		if strings.Contains(q, "attempts=attempts+1") {
			retry = true
			storedID = args[0].Value.(string)
		}
		if strings.Contains(q, "state='COMPENSATED'") {
			state = "COMPENSATED"
		}
		return driver.RowsAffected(1), nil
	}}
	db := sql.OpenDB(d)
	defer db.Close()
	g := &recordingGateway{releaseErr: errors.New("unavailable")}
	w := &Worker{DB: db, TenantID: "tenant", Gateway: g, GatewayApplicationID: "settlement"}
	item := job{id: "job-1", tenantID: "tenant", generation: 7}
	if err := w.reconcileEffect(context.Background(), item); err == nil {
		t.Fatal("failed release was swallowed")
	}
	if !retry || state != "INTENT" || storedID != "file-1" {
		t.Fatalf("failed cleanup lost retry state: retry=%v state=%s file=%s", retry, state, storedID)
	}
	if g.requestID != "request-original" || g.uploads != 0 || g.resolves != 0 || g.retires != 1 {
		t.Fatalf("unknown response must retire original key directly without reuploading retired content: %+v", g)
	}
	g.releaseErr = nil
	if err := w.reconcileEffect(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if state != "COMPENSATED" || g.uploads != 0 || g.releases != 2 {
		t.Fatalf("retry did not converge: state=%s uploads=%d releases=%d", state, g.uploads, g.releases)
	}
}

func TestCompensationFailureDoesNotStarveNewExports(t *testing.T) {
	claims := 0
	d := &exportDB{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
		if strings.Contains(q, "SELECT e.job_id") {
			return &exportRows{columns: 3, values: [][]driver.Value{{"old-job", int64(1), "tenant"}}}, nil
		}
		if strings.Contains(q, "SELECT id,tenant_id,report_type,lease_generation") {
			claims++
			if claims == 1 {
				return &exportRows{columns: 4, values: [][]driver.Value{{"new-job", "tenant", InvoicedReceivablesCSV, int64(0)}}}, nil
			}
			return &exportRows{columns: 4}, nil
		}
		return &exportRows{columns: 9}, nil
	}}
	db := sql.OpenDB(d)
	defer db.Close()
	w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker", GatewayMode: "legacy"}
	if err := w.RunOnce(context.Background()); err == nil {
		t.Fatal("unavailable compensating gateway was swallowed")
	}
	ready := false
	for _, q := range d.execs {
		if strings.Contains(q, "SET status='READY'") {
			ready = true
		}
	}
	if !ready {
		t.Fatal("durable compensation failure prevented unrelated export publication")
	}
}

func TestLeaseLostAfterUploadDoesNotBindOrPublish(t *testing.T) {
	renewals := 0
	d := &exportDB{exec: func(q string, _ []driver.NamedValue) (driver.Result, error) {
		if strings.Contains(q, "SET locked_until=") {
			renewals++
			if renewals == 5 {
				return driver.RowsAffected(0), nil
			}
		}
		return driver.RowsAffected(1), nil
	}}
	db := sql.OpenDB(d)
	defer db.Close()
	g := &recordingGateway{}
	w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker", Gateway: g, GatewayMode: "required", GatewayApplicationID: "settlement"}
	err := w.generate(context.Background(), job{id: "job-1", tenantID: "tenant", generation: 7})
	if !errors.Is(err, errLeaseLost) || g.uploads != 1 || g.binds != 0 {
		t.Fatalf("lost lease continued gateway work: err=%v uploads=%d binds=%d", err, g.uploads, g.binds)
	}
	for _, q := range d.execs {
		if strings.Contains(q, "SET status='READY'") {
			t.Fatal("lost lease published stale file")
		}
	}
}

func TestGatewayFailureRetainsIntentAndRequiredOrDualBehavior(t *testing.T) {
	for _, mode := range []string{"required", "dual"} {
		t.Run(mode, func(t *testing.T) {
			d := &exportDB{}
			db := sql.OpenDB(d)
			defer db.Close()
			g := &recordingGateway{bindErr: errors.New("bind response lost")}
			w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker", Gateway: g, GatewayMode: mode, GatewayApplicationID: "settlement"}
			err := w.generate(context.Background(), job{id: "job-1", tenantID: "tenant", generation: 7})
			if (mode == "required") != (err != nil) {
				t.Fatalf("mode=%s error=%v", mode, err)
			}
			ready, failed := false, false
			for i, q := range d.execs {
				if strings.Contains(q, "state='PUBLISHED'") {
					t.Fatal("failed gateway file was marked published")
				}
				if strings.Contains(q, "SET status='READY'") {
					ready = true
					if d.args[i][2].Value != "" || d.args[i][6].Value != "FAILED" {
						t.Fatal("dual fallback advertised failed gateway file")
					}
				}
				if strings.Contains(q, "SET status='FAILED'") {
					failed = true
				}
			}
			if ready != (mode == "dual") || failed != (mode == "required") {
				t.Fatalf("incorrect fallback: ready=%v failed=%v", ready, failed)
			}
		})
	}
}

func TestReconciliationDoesNotReleaseActiveOrPublishedGeneration(t *testing.T) {
	for _, status := range []string{"PROCESSING", "READY"} {
		t.Run(status, func(t *testing.T) {
			d := &exportDB{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(q, "SELECT status,lease_generation") {
					return &exportRows{columns: 4, values: [][]driver.Value{{status, int64(7), "file-1", true}}}, nil
				}
				return &exportRows{columns: 6, values: [][]driver.Value{{"settlement", "request", "export.csv", []byte("content"), "file-1", "INTENT"}}}, nil
			}}
			db := sql.OpenDB(d)
			defer db.Close()
			g := &recordingGateway{}
			w := &Worker{DB: db, TenantID: "tenant", Gateway: g, GatewayApplicationID: "settlement"}
			if err := w.reconcileEffect(context.Background(), job{id: "job-1", tenantID: "tenant", generation: 7}); err != nil {
				t.Fatal(err)
			}
			if g.uploads != 0 || g.releases != 0 {
				t.Fatal("current generation file was compensated")
			}
		})
	}
}
