package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	core "github.com/J-S-Te/license-core"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/config"
)

// The database boundary deliberately fails the document write. The invariant is
// a committed recovery record before any gateway side effect, not mock success.
type archiveFailureConnector struct {
	durable                        bool
	externalWithoutIntent          bool
	effect                         []driver.Value
	uploads                        int
	binds, locks, commits          int
	failDocument                   bool
	onLock                         func(int)
	onUpload, onDocument, onIntent func()
}
type archiveFailureConn struct{ c *archiveFailureConnector }

func (c *archiveFailureConnector) Connect(context.Context) (driver.Conn, error) {
	return &archiveFailureConn{c}, nil
}
func (c *archiveFailureConnector) Driver() driver.Driver { return taxDetailDriver{} }
func (c *archiveFailureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *archiveFailureConn) Close() error              { return nil }
func (c *archiveFailureConn) Begin() (driver.Tx, error) { return c, nil }
func (c *archiveFailureConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}
func (c *archiveFailureConn) Commit() error   { c.c.durable = true; c.c.commits++; return nil }
func (c *archiveFailureConn) Rollback() error { return nil }
func (c *archiveFailureConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "FROM settlement_invoice_archive_effect WHERE id=") && c.c.effect != nil {
		return &taxDetailRows{rows: [][]driver.Value{c.c.effect}}, nil
	}
	if strings.Contains(q, "COUNT(*) FROM settlement_tax_invoice") {
		return &taxDetailRows{rows: [][]driver.Value{{int64(1)}}}, nil
	}
	if strings.Contains(q, "SELECT id FROM settlement_tax_invoice") {
		c.c.locks++
		if c.c.onLock != nil {
			c.c.onLock(c.c.locks)
		}
		return &taxDetailRows{rows: [][]driver.Value{{"invoice"}}}, nil
	}
	return &taxDetailRows{}, nil
}
func (c *archiveFailureConn) ExecContext(_ context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(q, "INSERT INTO settlement_invoice_archive_effect(") {
		c.c.effect = []driver.Value{a[0].Value, a[3].Value, a[4].Value, a[5].Value, a[6].Value, a[7].Value, a[8].Value, a[9].Value, a[10].Value, a[11].Value, a[12].Value, a[13].Value}
		if c.c.onIntent != nil {
			c.c.onIntent()
		}
	}
	if strings.Contains(q, "INSERT INTO settlement_invoice_document(") {
		if c.c.onDocument != nil {
			c.c.onDocument()
		}
		if c.c.failDocument {
			return nil, errors.New("injected document write failure")
		}
	}
	return driver.RowsAffected(1), nil
}

type archiveFailureGateway struct{ c *archiveFailureConnector }

func (g archiveFailureGateway) Upload(context.Context, string, string, string, string, string, io.Reader) (string, error) {
	g.c.uploads++
	if !g.c.durable {
		g.c.externalWithoutIntent = true
	}
	if g.c.onUpload != nil {
		g.c.onUpload()
	}
	return "file", nil
}
func (g archiveFailureGateway) Bind(context.Context, string, string, string, string, string, string) error {
	g.c.binds++
	return nil
}
func (g archiveFailureGateway) Download(context.Context, string) ([]byte, error) { return nil, nil }
func (g archiveFailureGateway) RetireUpload(context.Context, string) (string, string, error) {
	return "file", "READY", nil
}
func (g archiveFailureGateway) Release(context.Context, string, string, string, string, string) error {
	return nil
}

func TestInvoiceArchivePersistsRecoveryBeforeGatewaySideEffects(t *testing.T) {
	c := &archiveFailureConnector{failDocument: true}
	db := sql.OpenDB(c)
	defer db.Close()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("file", "invoice.pdf")
	_, _ = part.Write([]byte("%PDF-1.4\ninvoice"))
	_ = form.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tax-invoices/invoice/documents", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Idempotency-Key", "key")
	req.Header.Set("Origin", "http://localhost")
	req.Header.Set("X-CSRF-Token", "1")
	response := httptest.NewRecorder()
	New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "http://localhost", OIDCTenantID: "dev", FileGatewayURL: "http://gateway", FileGatewayApplicationID: "settlement"}, nil, nil, nil, nil, archiveFailureGateway{c}, &archiveClockGate{now: time.Unix(100, 0), expires: time.Unix(101, 0)}).ServeHTTP(response, req)
	if response.Code < 400 {
		t.Fatalf("write failure returned %d", response.Code)
	}
	if c.externalWithoutIntent {
		t.Fatal("gateway upload/binding escaped without a committed recovery intent after document transaction failed")
	}
	if c.uploads != 1 {
		t.Fatalf("expected document write failure after a real handler upload; uploads=%d body=%s", c.uploads, response.Body.String())
	}
}

type archiveClockGate struct{ now, expires time.Time }

func (g *archiveClockGate) Check(context.Context, core.Operation) error {
	if !g.now.Before(g.expires) {
		return core.ErrDenied
	}
	return nil
}

func TestInvoiceArchiveRechecksLicenseAcrossWaitAndGatewayBoundaries(t *testing.T) {
	for _, stage := range []string{"missing-gate", "prepare-lock", "intent-write", "execution-lock", "upload", "before-commit"} {
		t.Run(stage, func(t *testing.T) {
			gate := &archiveClockGate{now: time.Unix(100, 0), expires: time.Unix(101, 0)}
			expire := func() { gate.now = gate.expires }
			c := &archiveFailureConnector{}
			switch stage {
			case "prepare-lock":
				c.onLock = func(n int) {
					if n == 1 {
						expire()
					}
				}
			case "intent-write":
				c.onIntent = expire
			case "execution-lock":
				c.onLock = func(n int) {
					if n == 2 {
						expire()
					}
				}
			case "upload":
				c.onUpload = expire
			case "before-commit":
				c.onDocument = expire
			}
			db := sql.OpenDB(c)
			defer db.Close()
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			part, _ := form.CreateFormFile("file", "invoice.pdf")
			_, _ = part.Write([]byte("%PDF-1.7\n%%EOF\n"))
			_ = form.Close()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/tax-invoices/invoice/documents", &body)
			request.Header.Set("Content-Type", form.FormDataContentType())
			request.Header.Set("Idempotency-Key", "key")
			request.Header.Set("Origin", "http://localhost")
			request.Header.Set("X-CSRF-Token", "1")
			response := httptest.NewRecorder()
			var gateDependency any = gate
			if stage == "missing-gate" {
				gateDependency = nil
			}
			New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "http://localhost", OIDCTenantID: "dev", FileGatewayURL: "http://gateway", FileGatewayApplicationID: "settlement"}, nil, nil, nil, nil, archiveFailureGateway{c}, gateDependency).ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "COMMERCIAL_LICENSE_DENIED") {
				t.Fatalf("expired at %s status=%d body=%s uploads=%d binds=%d commits=%d", stage, response.Code, response.Body.String(), c.uploads, c.binds, c.commits)
			}
			wantUploads, wantBinds, wantCommits := 0, 0, 1
			if stage == "missing-gate" || stage == "prepare-lock" || stage == "intent-write" {
				wantCommits = 0
			}
			if stage == "upload" || stage == "before-commit" {
				wantUploads = 1
			}
			if stage == "before-commit" {
				wantBinds = 1
			}
			if c.uploads != wantUploads || c.binds != wantBinds || c.commits != wantCommits {
				t.Fatalf("expired request crossed boundary: uploads=%d binds=%d commits=%d", c.uploads, c.binds, c.commits)
			}
		})
	}
}
