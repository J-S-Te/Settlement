package httpapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
)

func TestFinancialPaginationBounds(t *testing.T) {
	for _, c := range []struct {
		query         string
		limit, offset int
		invalid       bool
	}{
		{"", 100, 0, false}, {"?limit=50&offset=150", 50, 150, false}, {"?limit=1&offset=100000", 1, 100000, false},
		{"?limit=0", 0, 0, true}, {"?limit=101", 0, 0, true}, {"?offset=-1", 0, 0, true}, {"?offset=100001", 0, 0, true}, {"?limit=x", 0, 0, true}, {"?offset=1.5", 0, 0, true},
	} {
		t.Run(c.query, func(t *testing.T) {
			limit, offset, err := page(httptest.NewRequest("GET", "/api/v1/receipts"+c.query, nil))
			if (err != nil) != c.invalid || (!c.invalid && (limit != c.limit || offset != c.offset)) {
				t.Fatalf("got %d/%d %v", limit, offset, err)
			}
		})
	}
}

type pageConnector struct {
	t       *testing.T
	queried bool
}
type pageConn struct{ c *pageConnector }
type pageDriver struct{}
type emptyPageRows struct{}

func (c *pageConnector) Connect(context.Context) (driver.Conn, error) { return &pageConn{c}, nil }
func (c *pageConnector) Driver() driver.Driver                        { return pageDriver{} }
func (pageDriver) Open(string) (driver.Conn, error)                   { return nil, errors.New("use connector") }
func (c *pageConn) Prepare(string) (driver.Stmt, error)               { return nil, errors.New("unexpected prepare") }
func (c *pageConn) Close() error                                      { return nil }
func (c *pageConn) Begin() (driver.Tx, error)                         { return nil, errors.New("no transaction expected") }
func (c *pageConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.c.queried = true
	if !strings.Contains(q, "LIMIT ? OFFSET ?") || !strings.Contains(q, "tenant_id=?") || !strings.Contains(q, "ORDER BY") || !strings.Contains(q, "id") {
		c.c.t.Fatalf("unbounded/unstable query: %s", q)
	}
	if len(args) < 3 || args[0].Value != "dev" || args[len(args)-2].Value != int64(17) || args[len(args)-1].Value != int64(102) {
		c.c.t.Fatalf("args=%v", args)
	}
	if strings.Contains(q, "settlement_local_notification") && (len(args) != 4 || args[1].Value != "dev-finance") {
		c.c.t.Fatalf("notification recipient scope=%v", args)
	}
	return emptyPageRows{}, nil
}
func (emptyPageRows) Columns() []string         { return []string{"unused"} }
func (emptyPageRows) Close() error              { return nil }
func (emptyPageRows) Next([]driver.Value) error { return io.EOF }

func TestFinancialListsApplyDatabasePagination(t *testing.T) {
	for _, path := range []string{"receivable-plans", "receipts", "receipt-allocations", "invoice-requests", "tax-invoices", "dunning/cases", "dunning/actions", "dunning/policies", "notifications"} {
		t.Run(path, func(t *testing.T) {
			connector := &pageConnector{t: t}
			db := sql.OpenDB(connector)
			defer db.Close()
			h := New(db, config.Config{DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/"+path+"?limit=17&offset=102", nil))
			if w.Code != 200 || !connector.queried || strings.TrimSpace(w.Body.String()) != `{"data":[]}` {
				t.Fatalf("response=%d %s queried=%v", w.Code, w.Body.String(), connector.queried)
			}
		})
	}
}
