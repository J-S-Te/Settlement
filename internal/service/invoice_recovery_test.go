package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

type recoveryStep struct {
	sql    string
	values []driver.Value
	err    error
}
type recoveryConnector struct {
	t                     *testing.T
	steps                 []recoveryStep
	committed, rolledBack bool
}
type recoveryConn struct{ script *recoveryConnector }
type recoveryRows struct {
	values []driver.Value
	read   bool
}

func (c *recoveryConnector) Connect(context.Context) (driver.Conn, error) {
	return &recoveryConn{c}, nil
}
func (c *recoveryConnector) Driver() driver.Driver { return recoveryDriver{} }

type recoveryDriver struct{}

func (recoveryDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }
func (c *recoveryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *recoveryConn) Close() error              { return nil }
func (c *recoveryConn) Begin() (driver.Tx, error) { return c, nil }
func (c *recoveryConn) Commit() error             { c.script.committed = true; return nil }
func (c *recoveryConn) Rollback() error           { c.script.rolledBack = true; return nil }
func (c *recoveryConn) take(q string) recoveryStep {
	if len(c.script.steps) == 0 {
		c.script.t.Fatalf("unexpected SQL %s", q)
	}
	s := c.script.steps[0]
	c.script.steps = c.script.steps[1:]
	if !strings.Contains(q, s.sql) {
		c.script.t.Fatalf("SQL %s does not contain %s", q, s.sql)
	}
	return s
}
func (c *recoveryConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	s := c.take(q)
	if strings.Contains(q, "settlement_outbox_event") {
		if !strings.Contains(string(args[7].Value.([]byte)), "failed_attempt_id") {
			c.script.t.Fatal("audit must preserve failed attempt")
		}
		if args[3].Value != "PLATFORM_AUDIT" || args[4].Value != "SETTLEMENT_INVOICE_FAILURE_REVIEWED" {
			c.script.t.Fatal("only audit event permitted; no tax command")
		}
	}
	return driver.RowsAffected(1), s.err
}
func (c *recoveryConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	s := c.take(q)
	return &recoveryRows{values: s.values}, s.err
}
func (r *recoveryRows) Columns() []string {
	columns := make([]string, len(r.values))
	for i := range columns {
		columns[i] = "value"
	}
	return columns
}
func (r *recoveryRows) Close() error { return nil }
func (r *recoveryRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	copy(dest, r.values)
	r.read = true
	return nil
}

func TestRecoverFailedInvoiceTransaction(t *testing.T) {
	for _, scenario := range []struct {
		name, requestStatus, attemptStatus, actor string
		invoices, conflicts                       int64
		want                                      error
	}{
		{"definite failure", "ISSUE_FAILED", "ISSUE_FAILED", "reviewer", 0, 0, nil},
		{"unknown request", "ISSUE_UNKNOWN", "UNKNOWN", "reviewer", 0, 0, ErrConflict},
		{"applicant separated", "ISSUE_FAILED", "ISSUE_FAILED", "applicant", 0, 0, ErrInvalid},
		{"unknown attempt", "ISSUE_FAILED", "UNKNOWN", "reviewer", 0, 0, ErrConflict},
		{"existing invoice", "ISSUE_FAILED", "ISSUE_FAILED", "reviewer", 1, 0, ErrConflict},
		{"provider conflict", "ISSUE_FAILED", "ISSUE_FAILED", "reviewer", 0, 1, ErrConflict},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			steps := []recoveryStep{{sql: "INSERT INTO settlement_idempotency_record"}, {sql: "FOR UPDATE", values: []driver.Value{scenario.requestStatus, "applicant", int64(3)}}}
			if scenario.requestStatus == "ISSUE_FAILED" && scenario.actor != "applicant" {
				steps = append(steps, recoveryStep{sql: "LIMIT 1 FOR UPDATE", values: []driver.Value{"attempt-1", scenario.attemptStatus, "TAX_ADAPTER", int64(1), "failure-event", "provider"}})
				if scenario.attemptStatus == "ISSUE_FAILED" {
					steps = append(steps, recoveryStep{sql: "FROM settlement_tax_invoice", values: []driver.Value{scenario.invoices}})
					if scenario.invoices == 0 {
						steps = append(steps, recoveryStep{sql: "PROVIDER_CONFLICT", values: []driver.Value{scenario.conflicts}})
					}
				}
			}
			if scenario.want == nil {
				steps = append(steps, recoveryStep{sql: "status='SUBMITTED',approved_by=''"}, recoveryStep{sql: "INSERT INTO settlement_outbox_event"}, recoveryStep{sql: "INSERT INTO settlement_audit_event"})
			}
			connector := &recoveryConnector{t: t, steps: steps}
			db := sql.OpenDB(connector)
			defer db.Close()
			s := &Service{DB: db, InvoiceIssuanceMode: "tax_adapter", TaxProviderCode: "provider"}
			err := s.RecoverFailedInvoice(context.Background(), Principal{TenantID: "tenant-a", UserID: scenario.actor}, "key-1", "request-1", 3, "核实税控失败结果")
			if !errors.Is(err, scenario.want) {
				t.Fatalf("error=%v want=%v", err, scenario.want)
			}
			if len(connector.steps) != 0 || connector.committed != (scenario.want == nil) || connector.rolledBack != (scenario.want != nil) {
				t.Fatalf("transaction not complete: %+v", connector)
			}
		})
	}
}

func TestRecoverFailedInvoiceRejectsBeforeDatabase(t *testing.T) {
	s := &Service{}
	for _, reason := range []string{"", "短", strings.Repeat("字", 1001)} {
		if err := s.RecoverFailedInvoice(context.Background(), Principal{TenantID: "t", UserID: "u"}, "key", "id", 1, reason); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}
func TestOnlyAuthoritativeFailureRecoverable(t *testing.T) {
	for _, status := range []string{"PENDING", "ACCEPTED", "PROCESSING", "UNKNOWN", "ISSUED"} {
		if recoverableInvoiceFailure(status, "TAX_ADAPTER", 1, "event") {
			t.Fatal(status)
		}
	}
	if recoverableInvoiceFailure("ISSUE_FAILED", "MANUAL", 1, "event") || recoverableInvoiceFailure("ISSUE_FAILED", "TAX_ADAPTER", 0, "event") || recoverableInvoiceFailure("ISSUE_FAILED", "TAX_ADAPTER", 1, "") {
		t.Fatal("unproven failure allowed")
	}
}

func TestFailedInvoiceRecoveryReplayDoesNotMutateOrReaudit(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"actor_id": "reviewer", "id": "request-1", "version": 3, "reason": "核实税控失败结果"})
	hash := sha256.Sum256(body)
	connector := &recoveryConnector{t: t, steps: []recoveryStep{
		{sql: "INSERT INTO settlement_idempotency_record", err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		{sql: "SELECT request_hash,resource_id", values: []driver.Value{hash[:], "request-1"}},
	}}
	db := sql.OpenDB(connector)
	defer db.Close()
	err := (&Service{DB: db}).RecoverFailedInvoice(context.Background(), Principal{TenantID: "tenant-a", UserID: "reviewer"}, "key-1", "request-1", 3, "核实税控失败结果")
	if err != nil || !connector.committed || len(connector.steps) != 0 {
		t.Fatalf("replay error=%v script=%+v", err, connector)
	}
}

func TestFailedInvoiceRecoveryIdempotencyBodyMismatch(t *testing.T) {
	connector := &recoveryConnector{t: t, steps: []recoveryStep{
		{sql: "INSERT INTO settlement_idempotency_record", err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		{sql: "SELECT request_hash,resource_id", values: []driver.Value{[]byte("different-hash"), "request-1"}},
	}}
	db := sql.OpenDB(connector)
	defer db.Close()
	err := (&Service{DB: db}).RecoverFailedInvoice(context.Background(), Principal{TenantID: "tenant-a", UserID: "reviewer"}, "key-1", "request-1", 3, "核实税控失败结果")
	if !errors.Is(err, ErrConflict) || connector.committed || !connector.rolledBack {
		t.Fatalf("conflicting replay error=%v", err)
	}
}
