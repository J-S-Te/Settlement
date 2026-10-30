package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// 这些测试编排信用补偿路径使用的事务，
// 证明选择了哪个结清事实及其取消或
// 补偿方式，不打开业务数据库。

type creditReversalQueryStep struct {
	match  string
	check  func(query string, args []driver.NamedValue) error
	values []driver.Value
}

type creditReversalExecStep struct {
	match string
	check func(args []driver.NamedValue) error
}

type creditReversalConnector struct {
	t         *testing.T
	queries   []creditReversalQueryStep
	execs     []creditReversalExecStep
	usedQuery int
	usedExec  int
	committed bool
}

type creditReversalConn struct{ connector *creditReversalConnector }

func (c *creditReversalConnector) Connect(context.Context) (driver.Conn, error) {
	return &creditReversalConn{c}, nil
}
func (c *creditReversalConnector) Driver() driver.Driver { return creditReversalDriver{} }

type creditReversalDriver struct{}

func (creditReversalDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

func (c *creditReversalConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *creditReversalConn) Close() error              { return nil }
func (c *creditReversalConn) Begin() (driver.Tx, error) { return c, nil }
func (c *creditReversalConn) Commit() error             { c.connector.committed = true; return nil }
func (c *creditReversalConn) Rollback() error           { return nil }

func (c *creditReversalConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	connector := c.connector
	if connector.usedQuery >= len(connector.queries) {
		connector.t.Fatalf("unexpected query: %s", query)
	}
	step := connector.queries[connector.usedQuery]
	connector.usedQuery++
	if !strings.Contains(query, step.match) {
		connector.t.Fatalf("query %q does not contain %q", query, step.match)
	}
	if step.check != nil {
		if err := step.check(query, args); err != nil {
			connector.t.Fatal(err)
		}
	}
	return &creditReversalRows{values: step.values}, nil
}

func (c *creditReversalConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	connector := c.connector
	if connector.usedExec >= len(connector.execs) {
		connector.t.Fatalf("unexpected exec: %s", query)
	}
	step := connector.execs[connector.usedExec]
	connector.usedExec++
	if !strings.Contains(query, step.match) {
		connector.t.Fatalf("exec %q does not contain %q", query, step.match)
	}
	if step.check != nil {
		if err := step.check(args); err != nil {
			connector.t.Fatal(err)
		}
	}
	return driver.RowsAffected(1), nil
}

type creditReversalRows struct {
	values []driver.Value
	read   bool
}

func (r *creditReversalRows) Columns() []string {
	if len(r.values) == 0 {
		return []string{"unused"}
	}
	columns := make([]string, len(r.values))
	for index := range columns {
		columns[index] = fmt.Sprintf("column_%d", index)
	}
	return columns
}
func (r *creditReversalRows) Close() error { return nil }
func (r *creditReversalRows) Next(dest []driver.Value) error {
	if r.read || len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values)
	r.read = true
	return nil
}

func completedCreditPayload(t *testing.T, allocationID string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"event_id":      "EVENT-" + allocationID,
		"event_type":    creditEventCompleted,
		"tenant_id":     "tenant-a",
		"payment_id":    allocationID,
		"receipt_id":    "RECEIPT-1",
		"allocation_id": allocationID,
		"customer_id":   uint64(42),
		"contract_no":   "HT-1",
		"period_no":     "1",
		"due_date":      "2099-12-31T00:00:00Z",
		"paid_date":     "2026-10-18T00:00:00Z",
		"due_amount":    "100.00",
		"paid_amount":   "100.00",
		"source_system": "settlement",
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func assertQueryIdentity(query string, args []driver.NamedValue) error {
	for _, fragment := range []string{"FROM settlement_credit_sync", "receivable_id=?", "event_type=?", "NOT EXISTS", "ORDER BY"} {
		if !strings.Contains(query, fragment) {
			return fmt.Errorf("compensation query must locate the receivable cycle and exclude compensated facts, missing %q: %s", fragment, query)
		}
	}
	if len(args) != 4 {
		return fmt.Errorf("compensation query args=%v, want tenant, receivable, completed type and reversed type", args)
	}
	want := []string{"tenant-a", "RECV-1", creditEventCompleted, creditEventReversed}
	for index, value := range want {
		got, ok := args[index].Value.(string)
		if !ok || got != value {
			return fmt.Errorf("compensation query arg %d=%v, want %q", index, args[index].Value, value)
		}
	}
	return nil
}

func openCreditReversalTx(t *testing.T, connector *creditReversalConnector) (*sql.DB, *sql.Tx) {
	t.Helper()
	db := sql.OpenDB(connector)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return db, tx
}

func TestCreditReversalCancelsUndeliveredCompletionByReceivable(t *testing.T) {
	for _, status := range []string{"PENDING", "RETRY_WAIT"} {
		t.Run(status, func(t *testing.T) {
			payload := completedCreditPayload(t, "ALLOC-B")
			connector := &creditReversalConnector{
				t: t,
				queries: []creditReversalQueryStep{{
					match:  "FROM settlement_credit_sync",
					check:  assertQueryIdentity,
					values: []driver.Value{"EVENT-B", creditEventCompleted, status, "ALLOC-B", payload},
				}},
				execs: []creditReversalExecStep{{
					match: "UPDATE settlement_credit_sync SET status='CANCELLED'",
					check: func(args []driver.NamedValue) error {
						if len(args) != 2 || args[0].Value != "tenant-a" || args[1].Value != "EVENT-B" {
							return fmt.Errorf("cancel args=%v", args)
						}
						return nil
					},
				}},
			}
			db, tx := openCreditReversalTx(t, connector)
			defer db.Close()
			reversalEventID, err := enqueueCreditReversal(context.Background(), tx, "tenant-a", "ALLOC-A", "RECV-1")
			if err != nil {
				t.Fatal(err)
			}
			if reversalEventID != "" {
				t.Fatalf("undelivered completion should be cancelled, got reversal event %q", reversalEventID)
			}
			if connector.usedQuery != 1 || connector.usedExec != 1 {
				t.Fatalf("query/exec=%d/%d", connector.usedQuery, connector.usedExec)
			}
			if err = tx.Commit(); err != nil || !connector.committed {
				t.Fatalf("commit err=%v committed=%v", err, connector.committed)
			}
		})
	}
}

// TestCreditReversalCompensatesDeliveredCompletionByIdentity 验证CreditReversalCompensatesDeliveredCompletionByIdentity的业务约束与回归行为。
// 参数：t为测试上下文，使用独立夹具。
// 返回值：无；预期结果或状态不一致时测试失败。
func TestCreditReversalCompensatesDeliveredCompletionByIdentity(t *testing.T) {
	payload := completedCreditPayload(t, "ALLOC-B")
	var insertedEventID string
	connector := &creditReversalConnector{
		t: t,
		queries: []creditReversalQueryStep{{
			match:  "FROM settlement_credit_sync",
			check:  assertQueryIdentity,
			values: []driver.Value{"EVENT-B", creditEventCompleted, "DELIVERED", "ALLOC-B", payload},
		}},
		execs: []creditReversalExecStep{{
			match: "INSERT INTO settlement_credit_sync",
			check: func(args []driver.NamedValue) error {
				if len(args) != 10 {
					return fmt.Errorf("reversal insert args=%v", args)
				}
				insertedEventID = fmt.Sprint(args[2].Value)
				want := map[int]string{1: "tenant-a", 3: creditEventReversed, 4: "EVENT-B", 5: "ALLOC-A", 6: "RECV-1"}
				for index, value := range want {
					if got, ok := args[index].Value.(string); !ok || got != value {
						return fmt.Errorf("reversal insert arg %d=%v, want %q", index, args[index].Value, value)
					}
				}
				raw, ok := args[9].Value.([]byte)
				if !ok {
					return fmt.Errorf("reversal payload type %T", args[9].Value)
				}
				var event map[string]any
				if err := json.Unmarshal(raw, &event); err != nil {
					return err
				}
				for field, value := range map[string]string{
					"event_type":             creditEventReversed,
					"reversal_of_payment_id": "ALLOC-B",
					"paid_amount":            "0.00",
					"due_amount":             "100.00",
					"period_no":              "1",
					"contract_no":            "HT-1",
				} {
					if got, ok := event[field].(string); !ok || got != value {
						return fmt.Errorf("reversal payload %s=%v, want %q", field, event[field], value)
					}
				}
				if got, ok := event["customer_id"].(float64); !ok || got != 42 {
					return fmt.Errorf("reversal payload customer_id=%v", event["customer_id"])
				}
				return nil
			},
		}},
	}
	db, tx := openCreditReversalTx(t, connector)
	defer db.Close()
	reversalEventID, err := enqueueCreditReversal(context.Background(), tx, "tenant-a", "ALLOC-A", "RECV-1")
	if err != nil {
		t.Fatal(err)
	}
	if reversalEventID == "" || reversalEventID != insertedEventID {
		t.Fatalf("reversal event id=%q inserted=%q", reversalEventID, insertedEventID)
	}
	if connector.usedQuery != 1 || connector.usedExec != 1 {
		t.Fatalf("query/exec=%d/%d", connector.usedQuery, connector.usedExec)
	}
	if err = tx.Commit(); err != nil || !connector.committed {
		t.Fatalf("commit err=%v committed=%v", err, connector.committed)
	}
}

func TestCreditReversalSkipsFactsWithoutActiveCompletion(t *testing.T) {
	for _, name := range []string{"partially settled receivable", "already compensated completion"} {
		t.Run(name, func(t *testing.T) {
			connector := &creditReversalConnector{
				t: t,
				queries: []creditReversalQueryStep{{
					match: "FROM settlement_credit_sync",
					check: assertQueryIdentity,
					// 无记录：查询仅读取该应收有效且尚未补偿的
					// 完成事件。
				}},
			}
			db, tx := openCreditReversalTx(t, connector)
			defer db.Close()
			reversalEventID, err := enqueueCreditReversal(context.Background(), tx, "tenant-a", "ALLOC-A", "RECV-1")
			if err != nil {
				t.Fatal(err)
			}
			if reversalEventID != "" || connector.usedQuery != 1 || connector.usedExec != 0 {
				t.Fatalf("event=%q query/exec=%d/%d", reversalEventID, connector.usedQuery, connector.usedExec)
			}
			if err = tx.Commit(); err != nil || !connector.committed {
				t.Fatalf("commit err=%v committed=%v", err, connector.committed)
			}
		})
	}
}
