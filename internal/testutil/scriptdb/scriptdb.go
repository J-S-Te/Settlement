// Package scriptdb 仅为单元测试提供确定性 SQL 驱动。
// 它验证应用查询和事务边界，不验证 MySQL 行为。
package scriptdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type Step struct {
	Kind, SQL string
	Rows      [][]driver.Value
	Columns   int
	Err       error
	After     func()
}

type connector struct {
	t     *testing.T
	steps []Step
}

func Open(t *testing.T, steps ...Step) *sql.DB {
	t.Helper()
	c := &connector{t: t, steps: steps}
	db := sql.OpenDB(c)
	t.Cleanup(func() {
		_ = db.Close()
		if len(c.steps) != 0 {
			t.Errorf("unexecuted SQL boundary: %s %s", c.steps[0].Kind, c.steps[0].SQL)
		}
	})
	return db
}

func (c *connector) take(kind, query string) (Step, error) {
	if len(c.steps) == 0 {
		err := fmt.Errorf("unexpected %s: %s", kind, query)
		c.t.Error(err)
		return Step{}, err
	}
	s := c.steps[0]
	if kind != s.Kind || !strings.Contains(query, s.SQL) {
		err := fmt.Errorf("got %s %s; want %s containing %s", kind, query, s.Kind, s.SQL)
		c.t.Error(err)
		return Step{}, err
	}
	c.steps = c.steps[1:]
	if s.After != nil {
		s.After()
	}
	return s, s.Err
}

func (c *connector) Connect(context.Context) (driver.Conn, error) { return &conn{c}, nil }
func (c *connector) Driver() driver.Driver                        { return drv{} }

type drv struct{}

func (drv) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type conn struct{ c *connector }

func (c *conn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected prepare") }
func (c *conn) Close() error                        { return nil }
func (c *conn) Begin() (driver.Tx, error) {
	_, err := c.c.take("begin", "")
	return c, err
}

// Isolation semantics are verified by opt-in MySQL tests. This boundary driver
// records the transaction only; it must not silently exercise a different path
// merely because production requests an explicit isolation level.
func (c *conn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return c.Begin() }
func (c *conn) Commit() error                                                { _, err := c.c.take("commit", ""); return err }
func (c *conn) Rollback() error                                              { _, err := c.c.take("rollback", ""); return err }
func (c *conn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	_, err := c.c.take("exec", q)
	return driver.RowsAffected(1), err
}
func (c *conn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	s, err := c.c.take("query", q)
	return &rows{values: s.Rows, columns: s.Columns}, err
}

type rows struct {
	values  [][]driver.Value
	columns int
}

func (r *rows) Columns() []string {
	n := r.columns
	if len(r.values) > 0 {
		n = len(r.values[0])
	}
	return make([]string, n)
}
func (r *rows) Close() error { return nil }
func (r *rows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}
