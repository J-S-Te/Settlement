package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/filegatewayclient"
	"github.com/j-s-te/settlement/internal/migration"
	"github.com/j-s-te/settlement/migrations"
)

func invoiceArchiveMySQL(t *testing.T) (*sql.DB, *mysql.Config) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_TEST_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_TEST_DSN not configured; real MySQL invoice archive acceptance not executed")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !regexp.MustCompile(`^settlement_e2e(_[a-z0-9_]+)?$`).MatchString(cfg.DBName) {
		t.Fatal("SETTLEMENT_TEST_DSN must identify settlement_e2e[_namespace]; DSN withheld")
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "settlement_e2e_archive_" + hex.EncodeToString(nonce[:])
	controlCfg := cfg.Clone()
	controlCfg.DBName = ""
	control, err := sql.Open("mysql", controlCfg.FormatDSN())
	if err != nil {
		t.Fatal("open test control connection failed")
	}
	t.Cleanup(func() { control.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err = control.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create owned invoice archive database failed; DSN withheld")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := control.ExecContext(ctx, "DROP DATABASE `"+name+"`"); err != nil {
			t.Error("drop owned invoice archive database failed; DSN withheld")
		}
	})
	runCfg := cfg.Clone()
	runCfg.DBName = name
	runCfg.ParseTime = true
	runCfg.MultiStatements = true
	if err = migration.Run(ctx, runCfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatalf("apply real Settlement migrations: %v", err)
	}
	db, err := sql.Open("mysql", runCfg.FormatDSN())
	if err != nil {
		t.Fatal("open owned archive database failed")
	}
	t.Cleanup(func() { db.Close() })
	return db, runCfg
}

func seedArchiveInvoice(t *testing.T, db *sql.DB, tenant string) string {
	t.Helper()
	ctx := context.Background()
	snapshot, request, invoice := newID(), newID(), newID()
	// Deliberate isolated fixtures, retaining all real foreign keys and migrations.
	for _, step := range []struct {
		q string
		a []any
	}{
		{`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,'test',?,?,?,1,'customer','customer','CNY',1,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{snapshot, tenant, newID(), newID(), newID()}},
		{`INSERT INTO settlement_invoice_request(id,tenant_id,request_no,contract_snapshot_id,buyer_profile_snapshot,invoice_type,amount_excl_tax,tax_amount,amount_incl_tax,status,idempotency_key,submitted_by,created_at,updated_at) VALUES(?,?,?,?,'{}','VAT_NORMAL',1,0,1,'ISSUED',?,'test',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{request, tenant, newID(), snapshot, newID()}},
		{`INSERT INTO settlement_tax_invoice(id,tenant_id,invoice_request_id,invoice_code,invoice_no,invoice_type,currency,amount_excl_tax,tax_amount,amount_incl_tax,issue_date,buyer_profile_snapshot,seller_profile_snapshot,status,issued_by_channel,created_at) VALUES(?,?,?,'test',?,'VAT_NORMAL','CNY',1,0,1,'2026-10-23','{}','{}','ISSUED','MANUAL',UTC_TIMESTAMP(3))`, []any{invoice, tenant, request, newID()}},
	} {
		if _, err := db.ExecContext(ctx, step.q, step.a...); err != nil {
			t.Fatal(err)
		}
	}
	return invoice
}
func archiveInput(invoice, key string) InvoiceArchiveInput {
	content := []byte("%PDF-1.7\n%%EOF\n")
	return InvoiceArchiveInput{Key: key, InvoiceID: invoice, DocumentType: "ELECTRONIC_INVOICE", OriginalName: "invoice.pdf", MediaType: "application/pdf", Checksum: fmt.Sprintf("%x", sha256.Sum256(content)), Content: content}
}
func archivePrincipal(tenant string) Principal {
	return Principal{TenantID: tenant, UserID: "test", Permissions: map[string]bool{"settlement.invoice.issue": true}}
}
func dueArchiveEffects(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`UPDATE settlement_invoice_archive_effect SET available_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 SECOND) WHERE state='PENDING'`); err != nil {
		t.Fatal(err)
	}
}
func archiveState(t *testing.T, db *sql.DB, invoice string) string {
	t.Helper()
	var state string
	if err := db.QueryRow(`SELECT state FROM settlement_invoice_archive_effect WHERE tax_invoice_id=? ORDER BY created_at LIMIT 1`, invoice).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// Real HTTP client against a controlled boundary, not a real gateway acceptance.
// The opt-in test below runs the same recovery assertions against the real process.
type archiveHTTPBoundary struct {
	mu               sync.Mutex
	requests         map[string]string
	releases         int
	failRelease      bool
	entered, proceed chan struct{}
}

func (b *archiveHTTPBoundary) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v1/files":
		if b.entered != nil {
			close(b.entered)
			<-b.proceed
			b.entered = nil
		}
		id := r.Header.Get("X-Request-ID")
		if b.requests[id] == "" {
			b.requests[id] = "file-" + id
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"file_id": b.requests[id]}})
	case strings.HasSuffix(r.URL.Path, "/retire"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/file-upload-requests/"), "/retire")
		file := b.requests[id]
		status := "READY"
		if file == "" {
			status = "CANCELLED"
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"request_id": id, "file_id": file, "status": status, "retired": true}})
	case strings.HasSuffix(r.URL.Path, "/bindings/release"):
		if b.failRelease {
			w.WriteHeader(503)
			return
		}
		b.releases++
		w.WriteHeader(200)
	case strings.HasSuffix(r.URL.Path, "/bindings"):
		w.WriteHeader(200)
	default:
		w.WriteHeader(400)
	}
}
func boundaryArchiveGateway(t *testing.T, b *archiveHTTPBoundary) (*filegatewayclient.Client, InvoiceArchiveIdentity) {
	t.Helper()
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	g, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "boundary-machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	return g, InvoiceArchiveIdentity{TenantID: "tenant", ApplicationID: "settlement", GatewayURL: server.URL}
}

func TestMySQLInvoiceArchiveFailureRecoveryAndConcurrency(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	s := &Service{DB: db}
	b := &archiveHTTPBoundary{requests: map[string]string{}}
	g, identity := boundaryArchiveGateway(t, b)
	p := archivePrincipal(identity.TenantID)
	ctx := context.Background()
	failed := seedArchiveInvoice(t, db, p.TenantID)
	if _, err := db.Exec(`CREATE TRIGGER fail_archive_audit BEFORE INSERT ON settlement_outbox_event FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected archive audit failure'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UploadInvoiceDocument(ctx, p, identity, g, archiveInput(failed, "failed-key")); err == nil {
		t.Fatal("audit transaction failure was ignored")
	}
	if archiveState(t, db, failed) != "PENDING" {
		t.Fatal("failed transaction lost durable pending record")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_invoice_document`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("audit rollback left document count=%d err=%v", count, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_archive_audit`); err != nil {
		t.Fatal(err)
	}
	dueArchiveEffects(t, db)
	worker := &InvoiceArchiveWorker{DB: db, Identity: identity, Gateway: g}
	b.failRelease = true
	if err := worker.RunOnce(ctx); err == nil {
		t.Fatal("release failure swallowed")
	}
	if archiveState(t, db, failed) != "PENDING" {
		t.Fatal("unconfirmed release was forgotten")
	}
	b.failRelease = false
	dueArchiveEffects(t, db)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if archiveState(t, db, failed) != "RETIRED" {
		t.Fatal("recovery did not converge")
	}
	if _, _, err := s.UploadInvoiceDocument(ctx, p, identity, g, archiveInput(failed, "failed-key")); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired key reuse=%v", err)
	}
	if _, _, err := s.UploadInvoiceDocument(ctx, p, identity, g, archiveInput(failed, "new-key")); err != nil {
		t.Fatal(err)
	}
	success := seedArchiveInvoice(t, db, p.TenantID)
	in := archiveInput(success, "concurrent-key")
	var wg sync.WaitGroup
	results := make(chan string, 8)
	errs := make(chan error, 8)
	for n := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			next := in
			if n%2 == 1 {
				next.Key = fmt.Sprintf("other-key-%d", n)
			}
			id, _, err := s.UploadInvoiceDocument(ctx, p, identity, g, next)
			if err != nil {
				errs <- err
			} else {
				results <- id
			}
		}(n)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrConflict) {
			t.Error(err)
		}
	}
	var expected string
	for id := range results {
		if expected == "" {
			expected = id
		} else if id != expected {
			t.Fatal("same content produced distinct documents")
		}
	}
	if expected == "" {
		t.Fatal("all concurrent uploads failed")
	}
	// Another key may win the initial checksum reservation, making every
	// concurrent-key call conflict legitimately. Establish its durable ownership
	// on the original invoice before checking cross-invoice reuse.
	if id, _, err := s.UploadInvoiceDocument(ctx, p, identity, g, in); err != nil || id != expected {
		t.Fatalf("persist canonical key on original invoice: sameDocument=%v err=%v", id == expected, err)
	}
	other := seedArchiveInvoice(t, db, p.TenantID)
	if _, _, err := s.UploadInvoiceDocument(ctx, p, identity, g, archiveInput(other, "concurrent-key")); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key cross-invoice accepted: %v", err)
	}
	drift := identity
	drift.ApplicationID = "changed"
	if _, _, err := s.UploadInvoiceDocument(ctx, p, drift, g, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("config drift accepted: %v", err)
	}
	for _, dimension := range []string{"application", "origin"} {
		changed, next := identity, in
		next.Key = "new-key-after-" + dimension + "-change"
		if dimension == "application" {
			changed.ApplicationID = "other-app"
		} else {
			changed.GatewayURL = "http://different-gateway"
		}
		var before, after int
		if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_invoice_archive_effect WHERE tax_invoice_id=?`, success).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.UploadInvoiceDocument(ctx, p, changed, g, next); !errors.Is(err, ErrInvoiceArchiveProvenance) {
			t.Fatalf("new key relabeled original document after %s drift: %v", dimension, err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_invoice_archive_effect WHERE tax_invoice_id=?`, success).Scan(&after); err != nil || after != before {
			t.Fatalf("drift recorded an unverified identity: before=%d after=%d err=%v", before, after, err)
		}
	}
	legacy := seedArchiveInvoice(t, db, p.TenantID)
	legacyInput := archiveInput(legacy, "legacy-new-key")
	if _, err := s.ArchiveInvoiceDocument(ctx, p, legacy, legacyInput.DocumentType, legacyInput.OriginalName, legacyInput.MediaType, "historical-file-without-journal", legacyInput.Checksum); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UploadInvoiceDocument(ctx, p, identity, g, legacyInput); !errors.Is(err, ErrInvoiceArchiveProvenance) {
		t.Fatalf("legacy provenance was guessed: %v", err)
	}
	wrong := p
	wrong.TenantID = "foreign"
	if _, _, err := s.UploadInvoiceDocument(ctx, wrong, identity, g, in); !errors.Is(err, ErrForbidden) {
		t.Fatal("cross-tenant identity accepted")
	}
	dueArchiveEffects(t, db)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if archiveState(t, db, success) != "ARCHIVED" {
		t.Fatal("successful archive was released")
	}
}

// Commit executes on real MySQL, then the driver drops its response. This is a
// transport outcome test, not a pretend successful database transaction.
type lostCommitConnector struct {
	driver.Connector
	mu      sync.Mutex
	commits int
}

type loseArchiveHTTPResponse struct {
	base   http.RoundTripper
	suffix string
}

func (l loseArchiveHTTPResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := l.base.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, l.suffix) && response.StatusCode >= 200 && response.StatusCode < 300 {
		_ = response.Body.Close()
		return nil, errors.New("injected lost response after real gateway completed request")
	}
	return response, err
}

type lostCommitConn struct {
	driver.Conn
	c *lostCommitConnector
}
type lostCommitTx struct {
	driver.Tx
	c *lostCommitConnector
}

func (c *lostCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	return &lostCommitConn{conn, c}, err
}
func (c *lostCommitConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	return &lostCommitTx{tx, c.c}, err
}
func (c *lostCommitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	return &lostCommitTx{tx, c.c}, err
}
func (c *lostCommitTx) Commit() error {
	err := c.Tx.Commit()
	c.c.mu.Lock()
	defer c.c.mu.Unlock()
	c.c.commits++
	if err == nil && c.c.commits == 2 {
		return errors.New("injected lost COMMIT response after real commit")
	}
	return err
}
func TestMySQLInvoiceArchiveUnknownCommitDoesNotReleaseDocument(t *testing.T) {
	db, cfg := invoiceArchiveMySQL(t)
	base, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := sql.OpenDB(&lostCommitConnector{Connector: base})
	defer wrapped.Close()
	s := &Service{DB: wrapped}
	b := &archiveHTTPBoundary{requests: map[string]string{}}
	g, identity := boundaryArchiveGateway(t, b)
	p := archivePrincipal(identity.TenantID)
	invoice := seedArchiveInvoice(t, db, p.TenantID)
	in := archiveInput(invoice, "commit-key")
	if _, _, err = s.UploadInvoiceDocument(context.Background(), p, identity, g, in); err == nil {
		t.Fatal("lost COMMIT response was hidden")
	}
	if archiveState(t, db, invoice) != "ARCHIVED" {
		t.Fatal("real committed archive disappeared")
	}
	dueArchiveEffects(t, db)
	if err = (&InvoiceArchiveWorker{DB: db, Identity: identity, Gateway: g}).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	released := b.releases
	b.mu.Unlock()
	if released != 0 {
		t.Fatal("COMMIT result unknown released a genuinely committed document")
	}
	if _, _, err = (&Service{DB: db}).UploadInvoiceDocument(context.Background(), p, identity, g, in); err != nil {
		t.Fatalf("replay did not recover committed result: %v", err)
	}
}

func TestMySQLInvoiceArchiveRecoverySkipsInFlightTransaction(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	b := &archiveHTTPBoundary{requests: map[string]string{}, entered: make(chan struct{}), proceed: make(chan struct{})}
	g, identity := boundaryArchiveGateway(t, b)
	p, s := archivePrincipal(identity.TenantID), &Service{DB: db}
	invoice := seedArchiveInvoice(t, db, p.TenantID)
	in := archiveInput(invoice, "in-flight-key")
	if _, err := s.prepareInvoiceArchive(context.Background(), p, identity, in); err != nil {
		t.Fatal(err)
	}
	dueArchiveEffects(t, db)
	entered, proceed := b.entered, b.proceed
	done := make(chan error, 1)
	go func() { _, _, err := s.UploadInvoiceDocument(context.Background(), p, identity, g, in); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(proceed)
		t.Fatal("upload did not enter HTTP boundary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	found, err := (&InvoiceArchiveWorker{DB: db, Identity: identity, Gateway: g}).recoverOne(ctx)
	cancel()
	close(proceed)
	if archiveErr := <-done; archiveErr != nil {
		t.Fatal(archiveErr)
	}
	if found || err != nil {
		t.Fatalf("worker claimed active transaction: found=%v err=%v", found, err)
	}
	if archiveState(t, db, invoice) != "ARCHIVED" {
		t.Fatal("active upload lost archive")
	}
}

func TestRealGatewayInvoiceArchiveRecovery(t *testing.T) {
	endpoint, token, tenant, app := os.Getenv("SETTLEMENT_TEST_FILE_GATEWAY_URL"), os.Getenv("SETTLEMENT_TEST_FILE_GATEWAY_TOKEN"), os.Getenv("SETTLEMENT_TEST_FILE_GATEWAY_TENANT_ID"), os.Getenv("SETTLEMENT_TEST_FILE_GATEWAY_APPLICATION_ID")
	if endpoint == "" || token == "" || tenant == "" || app == "" {
		t.Skip("explicit isolated gateway URL/token/tenant/application not configured; real gateway acceptance not executed")
	}
	db, _ := invoiceArchiveMySQL(t)
	g, err := filegatewayclient.New(endpoint, &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, func(context.Context) (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: app, GatewayURL: endpoint}
	p := archivePrincipal(tenant)
	s := &Service{DB: db}
	ctx := context.Background()
	invoice := seedArchiveInvoice(t, db, tenant)
	in := archiveInput(invoice, "real-failure-key")
	if _, err = db.Exec(`CREATE TRIGGER fail_real_archive BEFORE INSERT ON settlement_invoice_document FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected real document failure'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.UploadInvoiceDocument(ctx, p, identity, g, in); err == nil {
		t.Fatal("document failure ignored")
	}
	var requestID string
	if err = db.QueryRow(`SELECT request_id FROM settlement_invoice_archive_effect WHERE tax_invoice_id=?`, invoice).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	fileID, status, found, err := g.ResolveUpload(ctx, requestID)
	if err != nil || !found || status != "READY" {
		t.Fatalf("real gateway did not complete upload before DB failure: found=%v status=%s err=%v", found, status, err)
	}
	assertRealInvoiceGatewaySQL(t, tenant, app, fileID, requestID, invoice, in.Content, false)
	if _, err = db.Exec(`DROP TRIGGER fail_real_archive`); err != nil {
		t.Fatal(err)
	}
	dueArchiveEffects(t, db)
	if err = (&InvoiceArchiveWorker{DB: db, Identity: identity, Gateway: g}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if archiveState(t, db, invoice) != "RETIRED" {
		t.Fatal("real gateway recovery did not converge")
	}
	assertRealInvoiceGatewaySQL(t, tenant, app, fileID, requestID, invoice, in.Content, true)
	if err = g.Bind(ctx, app, fileID, "tax_invoice", invoice, "electronic_invoice", in.OriginalName); err == nil {
		t.Fatal("late real gateway binding revived retired file")
	}
	if _, err = g.Upload(ctx, requestID, app, "INVOICE_DOCUMENT", in.OriginalName, in.MediaType, bytes.NewReader(in.Content)); err == nil {
		t.Fatal("retired request accepted a late upload")
	}
	// Without an ACTIVE resource binding the machine must lose download access.
	// Physical preservation is checked independently inside the explicit test
	// storage root, never by weakening the gateway authorization boundary.
	assertUnboundInvoiceDownloadDenied(t, endpoint, token, fileID)
	id, created, err := s.UploadInvoiceDocument(ctx, p, identity, g, archiveInput(invoice, "real-retry-key"))
	if err != nil || id == "" || !created {
		t.Fatalf("new immutable attempt could not archive: created=%v err=%v", created, err)
	}
	replay, created, err := s.UploadInvoiceDocument(ctx, p, identity, g, archiveInput(invoice, "real-retry-key"))
	if err != nil || replay != id || created {
		t.Fatalf("replay changed archive result: created=%v err=%v", created, err)
	}
	var stored string
	if err = db.QueryRow(`SELECT storage_object_id FROM settlement_invoice_document WHERE id=?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	data, err := g.Download(ctx, stored)
	if err != nil || !bytes.Equal(data, in.Content) {
		t.Fatalf("archived real file unreadable: %v", err)
	}
	for _, stage := range []string{"upload", "bind"} {
		t.Run("lost-"+stage+"-response", func(t *testing.T) {
			invoice := seedArchiveInvoice(t, db, tenant)
			input := archiveInput(invoice, "lost-"+stage)
			suffix := "/api/v1/files"
			if stage == "bind" {
				suffix = "/bindings"
			}
			losing, err := filegatewayclient.New(endpoint, &http.Client{Timeout: 8 * time.Second, Transport: loseArchiveHTTPResponse{base: http.DefaultTransport, suffix: suffix}}, func(context.Context) (string, error) { return token, nil })
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.UploadInvoiceDocument(ctx, p, identity, losing, input); err == nil {
				t.Fatal("lost response was hidden")
			}
			var request string
			if err = db.QueryRow(`SELECT request_id FROM settlement_invoice_archive_effect WHERE tax_invoice_id=?`, invoice).Scan(&request); err != nil {
				t.Fatal(err)
			}
			file, _, found, err := g.ResolveUpload(ctx, request)
			if err != nil || !found {
				t.Fatalf("real completed HTTP operation not visible: found=%v err=%v", found, err)
			}
			dueArchiveEffects(t, db)
			if err = (&InvoiceArchiveWorker{DB: db, Identity: identity, Gateway: g}).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if archiveState(t, db, invoice) != "RETIRED" {
				t.Fatal("unknown external result was not recovered")
			}
			assertRealInvoiceGatewaySQL(t, tenant, app, file, request, invoice, input.Content, true)
			assertUnboundInvoiceDownloadDenied(t, endpoint, token, file)
			if err = g.Bind(ctx, app, file, "tax_invoice", invoice, "electronic_invoice", input.OriginalName); err == nil {
				t.Fatal("late bind revived an upload after its response was lost")
			}
		})
	}
}

func assertRealInvoiceGatewaySQL(t *testing.T, tenant, app, file, request, invoice string, content []byte, retired bool) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("FILE_GATEWAY_TEST_DSN"))
	if dsn == "" {
		t.Log("FILE_GATEWAY_TEST_DSN absent; gateway SQL assertions not executed")
		return
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !regexp.MustCompile(`^file_gateway_(test|e2e)(_[a-z0-9_]+)?$`).MatchString(cfg.DBName) {
		t.Fatal("gateway SQL assertion DSN must name isolated file_gateway_test/e2e schema; DSN withheld")
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open explicit gateway test database failed")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	checksum := fmt.Sprintf("%x", sha256.Sum256(content))
	var namespace, purpose, retention, classification, sha, status, relativePath string
	err = db.QueryRowContext(ctx, `SELECT f.namespace,f.purpose,f.retention_class,f.classification,LOWER(HEX(v.sha256)),f.status,v.storage_relative_path FROM file_object f JOIN file_version v ON v.id=f.current_version_id AND v.file_id=f.id WHERE f.id=? AND f.tenant_id=? AND f.application_id=?`, file, tenant, app).Scan(&namespace, &purpose, &retention, &classification, &sha, &status, &relativePath)
	if err != nil || namespace != "settlement" || purpose != "invoice-document" || retention != "LONG_TERM" || classification != "CONFIDENTIAL" || sha != checksum || status != "READY" {
		t.Fatalf("real invoice lifecycle identity mismatch: namespace=%s purpose=%s retention=%s classification=%s status=%s shaMatches=%v err=%v", namespace, purpose, retention, classification, status, sha == checksum, err)
	}
	var active, releases int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_binding WHERE tenant_id=? AND application_id=? AND file_id=? AND resource_type='tax_invoice' AND resource_id=? AND binding_type='electronic_invoice' AND status='ACTIVE'`, tenant, app, file, invoice).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_binding_release WHERE tenant_id=? AND application_id=? AND file_id=? AND resource_type='tax_invoice' AND resource_id=? AND binding_type='electronic_invoice'`, tenant, app, file, invoice).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if retired {
		var guard bool
		if err = db.QueryRowContext(ctx, `SELECT retired FROM file_upload_request_guard WHERE tenant_id=? AND application_id=? AND request_id=?`, tenant, app, request).Scan(&guard); err != nil || !guard || active != 0 || releases != 1 {
			t.Fatalf("real retired tuple active=%d releases=%d guard=%v err=%v", active, releases, guard, err)
		}
		storageRoot := strings.TrimSpace(os.Getenv("FILE_GATEWAY_TEST_STORAGE_ROOT"))
		if storageRoot == "" {
			t.Log("FILE_GATEWAY_TEST_STORAGE_ROOT absent; physical retained bytes assertion not executed")
			return
		}
		if !filepath.IsAbs(storageRoot) || filepath.Clean(storageRoot) == string(filepath.Separator) || !filepath.IsLocal(relativePath) {
			t.Fatal("explicit test storage root/path is not safely confined")
		}
		root, err := os.OpenRoot(storageRoot)
		if err != nil {
			t.Fatal("open explicit gateway test storage root failed")
		}
		defer root.Close()
		f, err := root.Open(relativePath)
		if err != nil {
			t.Fatal("retired invoice bytes not present inside explicit test root")
		}
		defer f.Close()
		got, err := io.ReadAll(io.LimitReader(f, (10<<20)+1))
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("retired invoice physical bytes changed: size=%d err=%v", len(got), err)
		}
	} else if active != 1 || releases != 0 {
		t.Fatalf("real pre-recovery binding active=%d releases=%d", active, releases)
	}
}

func assertUnboundInvoiceDownloadDenied(t *testing.T, endpoint, token, file string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/api/v1/files/"+url.PathEscape(file)+"/content", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal("real gateway download authorization check failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unbound invoice download status=%d, want 403", response.StatusCode)
	}
}
