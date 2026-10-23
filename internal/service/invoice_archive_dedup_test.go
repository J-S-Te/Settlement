package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

// A persistence boundary whose document and provenance fixtures are independent
// of the SELECT projection, so omitting the provenance read remains observable.
type dedupConnector struct {
	source       []driver.Value
	inserts      int
	insertedFile string
}
type dedupConn struct{ c *dedupConnector }
type dedupDriver struct{}
type dedupRows struct {
	values  [][]driver.Value
	columns int
}

func (c *dedupConnector) Connect(context.Context) (driver.Conn, error) { return &dedupConn{c}, nil }
func (c *dedupConnector) Driver() driver.Driver                        { return dedupDriver{} }
func (dedupDriver) Open(string) (driver.Conn, error)                   { return nil, errors.New("use connector") }
func (c *dedupConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *dedupConn) Close() error                                                 { return nil }
func (c *dedupConn) Begin() (driver.Tx, error)                                    { return c, nil }
func (c *dedupConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return c, nil }
func (c *dedupConn) Commit() error                                                { return nil }
func (c *dedupConn) Rollback() error                                              { return nil }
func (c *dedupConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "FROM settlement_tax_invoice"):
		return &dedupRows{values: [][]driver.Value{{"invoice"}}}, nil
	case strings.Contains(q, "FROM settlement_invoice_document"):
		row := []driver.Value{"document"}
		if strings.Contains(q, "storage_object_id") {
			row = append(row, "file")
		}
		return &dedupRows{values: [][]driver.Value{row}}, nil
	case strings.Contains(q, "document_id=?"):
		rows := [][]driver.Value{}
		if c.c.source != nil {
			rows = append(rows, c.c.source)
		}
		return &dedupRows{values: rows, columns: 12}, nil
	default:
		return &dedupRows{columns: 12}, nil
	}
}
func (c *dedupConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(q, "INSERT INTO settlement_invoice_archive_effect") {
		c.c.inserts++
		// The durable alias must carry the original stable file identity.
		for _, a := range args {
			if a.Value == "file" {
				c.c.insertedFile = "file"
			}
		}
	}
	return driver.RowsAffected(1), nil
}
func (r *dedupRows) Columns() []string {
	n := r.columns
	if len(r.values) > 0 {
		n = len(r.values[0])
	}
	return make([]string, n)
}
func (r *dedupRows) Close() error { return nil }
func (r *dedupRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}

func TestInvoiceArchiveDedupRequiresOriginalGatewayIdentity(t *testing.T) {
	for _, scenario := range []string{"app-drift", "origin-drift", "legacy-without-journal", "file-mismatch", "same-identity"} {
		t.Run(scenario, func(t *testing.T) {
			in := archiveInput("invoice", "new-key")
			identity := InvoiceArchiveIdentity{TenantID: "tenant", ApplicationID: "app", GatewayURL: "http://original"}
			d := &dedupConnector{source: []driver.Value{"source", "invoice", "ELECTRONIC_INVOICE", in.Checksum, "invoice.pdf", "application/pdf", "app", "http://original", "original-request", "file", "document", "ARCHIVED"}}
			switch scenario {
			case "app-drift":
				identity.ApplicationID = "other-app"
			case "origin-drift":
				identity.GatewayURL = "http://other"
			case "legacy-without-journal":
				d.source = nil
			case "file-mismatch":
				d.source[9] = "different-file"
			}
			db := sql.OpenDB(d)
			defer db.Close()
			effect, err := (&Service{DB: db}).prepareInvoiceArchive(context.Background(), archivePrincipal("tenant"), identity, in)
			if scenario == "same-identity" {
				if err != nil || effect.documentID != "document" || effect.fileID != "file" || d.insertedFile != "file" {
					t.Fatalf("alias lost confirmed source: document=%s file=%s persistedFile=%s err=%v", effect.documentID, effect.fileID, d.insertedFile, err)
				}
				return
			}
			if !errors.Is(err, ErrConflict) || d.inserts != 0 {
				t.Fatalf("unverified duplicate recorded under current gateway: inserts=%d err=%v", d.inserts, err)
			}
		})
	}
}
