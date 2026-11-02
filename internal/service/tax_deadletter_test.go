package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

func taxDeadLetterPrincipal() Principal {
	return Principal{TenantID: "tenant-a", UserID: "finance-1", Permissions: map[string]bool{"settlement.invoice.issue": true}}
}

// TestTaxDeadLetterListRequiresIssuePermission 验证死信视图只向开票经办能力开放。
// 参数：t 测试上下文。
// 返回值：无；无权限仍访问数据库或返回数据时失败。
func TestTaxDeadLetterListRequiresIssuePermission(t *testing.T) {
	s := &Service{}
	if _, err := s.ListTaxCommandDeadLetters(context.Background(), Principal{TenantID: "tenant-a", UserID: "sales-1"}, "", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("list without permission err=%v want ErrForbidden", err)
	}
	if err := s.RetryTaxCommandDeadLetter(context.Background(), Principal{TenantID: "tenant-a", UserID: "sales-1"}, "key-1", "outbox-1", TaxCommandDeadLetterRetryInput{Reason: "核实税控命令死信"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("retry without permission err=%v want ErrForbidden", err)
	}
}

// TestTaxDeadLetterListMapsIssueAndRedFlushCommands 验证死信列表区分开票与红冲命令，
// 并把尝试、申请状态和失败原因返回给财务。
// 参数：t 测试上下文。
// 返回值：无；列表未按租户/申请过滤或字段映射错误时失败。
func TestTaxDeadLetterListMapsIssueAndRedFlushCommands(t *testing.T) {
	var queryArgs []driver.NamedValue
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "settlement_outbox_event", Args: &queryArgs, Rows: [][]driver.Value{
		{"outbox-1", "event-1", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "request-1", int64(7), "DELIVERY_FAILED", "终态失败", "2026-01-02T03:04:05.000000Z", "2026-01-01T03:04:05.000000Z", "attempt-1", "request-1", "PENDING", "", "", "", "ISSUE_PENDING", ""},
		{"outbox-2", "event-2", "SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED", "invoice-1", int64(3), "HTTP_422", "税控拒绝", "2026-01-03T03:04:05.000000Z", "2026-01-01T03:04:05.000000Z", "", "", "", "red-attempt-1", "red-request-1", "PENDING", "", "ISSUE_PENDING"},
	}})
	items, err := (&Service{DB: db}).ListTaxCommandDeadLetters(context.Background(), taxDeadLetterPrincipal(), "request-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(queryArgs) != 3 || queryArgs[0].Value != "tenant-a" || queryArgs[1].Value != "request-1" || queryArgs[2].Value != "request-1" {
		t.Fatalf("list query must be tenant and request bound: %+v", queryArgs)
	}
	if len(items) != 2 {
		t.Fatalf("items=%+v", items)
	}
	issue, red := items[0], items[1]
	if issue.OperationType != "INVOICE_ISSUE" || issue.AttemptID != "attempt-1" || issue.RequestID != "request-1" || issue.RequestStatus != "ISSUE_PENDING" || issue.AttemptStatus != "PENDING" || issue.LastErrorCode != "DELIVERY_FAILED" || issue.LastErrorSummary != "终态失败" || issue.AttemptCount != 7 {
		t.Fatalf("issue dead letter mapping=%+v", issue)
	}
	if red.OperationType != "RED_FLUSH" || red.AttemptID != "red-attempt-1" || red.RequestID != "red-request-1" || red.RequestStatus != "ISSUE_PENDING" || red.LastErrorCode != "HTTP_422" {
		t.Fatalf("red flush dead letter mapping=%+v", red)
	}
}

// TestRetryTaxCommandDeadLetterRejectsInvalidInputBeforeDatabase 验证重试必须带
// 幂等键和可审计原因，参数错误不得触碰事务。
// 参数：t 测试上下文。
// 返回值：无；空键、短原因或超长原因未被拒绝时失败。
func TestRetryTaxCommandDeadLetterRejectsInvalidInputBeforeDatabase(t *testing.T) {
	s := &Service{}
	for _, tc := range []struct{ key, id, reason string }{
		{"", "outbox-1", "核实税控命令死信"},
		{"key-1", "", "核实税控命令死信"},
		{"key-1", "outbox-1", "短"},
		{"key-1", "outbox-1", strings.Repeat("字", 1001)},
	} {
		if err := s.RetryTaxCommandDeadLetter(context.Background(), taxDeadLetterPrincipal(), tc.key, tc.id, TaxCommandDeadLetterRetryInput{Reason: tc.reason}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("key=%q id=%q reason=%d err=%v want ErrInvalid", tc.key, tc.id, len([]rune(tc.reason)), err)
		}
	}
}

func retryDeadLetterBegin() []scriptdb.Step {
	return []scriptdb.Step{{Kind: "begin"}, {Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"}}
}

// TestRetryTaxCommandDeadLetterStateMachine 验证人工重试只接受"命令从未被税控接受、
// 申请仍在开票中、命令未被更新尝试取代"的死信，其余状态一律拒绝。
// 参数：t 测试上下文。
// 返回值：无；越权状态被放行或错误分类不符时失败。
func TestRetryTaxCommandDeadLetterStateMachine(t *testing.T) {
	deadOutbox := func(status, eventType, requestID string) scriptdb.Step {
		payload := []byte(`{"attempt_id":"attempt-1"}`)
		if eventType == "SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED" {
			payload = []byte(`{"red_flush_attempt_id":"red-attempt-1"}`)
		}
		return scriptdb.Step{Kind: "query", SQL: "FROM settlement_outbox_event", Rows: [][]driver.Value{{"TAX_INVOICE_COMMAND", status, eventType, "event-1", aggregateTypeFor(eventType), payload, "FAILED", "终态失败"}}}
	}
	for _, tc := range []struct {
		name  string
		steps []scriptdb.Step
		want  error
	}{
		{
			name:  "command not found",
			steps: append(retryDeadLetterBegin(), scriptdb.Step{Kind: "query", SQL: "FROM settlement_outbox_event", Err: sql.ErrNoRows}),
			want:  ErrNotFound,
		},
		{
			name:  "not dead letter",
			steps: append(retryDeadLetterBegin(), deadOutbox("PENDING", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "request-1")),
			want:  ErrConflict,
		},
		{
			name:  "wrong destination",
			steps: append(retryDeadLetterBegin(), scriptdb.Step{Kind: "query", SQL: "FROM settlement_outbox_event", Rows: [][]driver.Value{{"PLATFORM_AUDIT", "DEAD_LETTER", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "event-1", "invoice_request", []byte(`{"attempt_id":"attempt-1"}`), "FAILED", "终态失败"}}}),
			want:  ErrConflict,
		},
		{
			name: "attempt replaced by newer command",
			steps: append(retryDeadLetterBegin(), deadOutbox("DEAD_LETTER", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "request-1"),
				scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_issue_attempt", Rows: [][]driver.Value{{"PENDING", true, "request-1", "event-newer", int64(1), "ISSUE_PENDING"}}}),
			want: ErrConflict,
		},
		{
			name: "attempt already accepted",
			steps: append(retryDeadLetterBegin(), deadOutbox("DEAD_LETTER", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "request-1"),
				scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_issue_attempt", Rows: [][]driver.Value{{"PENDING", false, "request-1", "event-1", int64(1), "ISSUE_PENDING"}}}),
			want: ErrConflict,
		},
		{
			name: "request already terminal",
			steps: append(retryDeadLetterBegin(), deadOutbox("DEAD_LETTER", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "request-1"),
				scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_issue_attempt", Rows: [][]driver.Value{{"PENDING", true, "request-1", "event-1", int64(1), "ISSUED"}}}),
			want: ErrConflict,
		},
		{
			name: "attempt superseded by a newer attempt",
			steps: append(retryDeadLetterBegin(), deadOutbox("DEAD_LETTER", "SETTLEMENT_INVOICE_ISSUE_REQUESTED", "request-1"),
				scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_issue_attempt", Rows: [][]driver.Value{{"PENDING", true, "request-1", "event-1", int64(1), "ISSUE_PENDING"}}},
				scriptdb.Step{Kind: "query", SQL: "newer.attempt_no>?", Rows: [][]driver.Value{{false}}}),
			want: ErrConflict,
		},
		{
			name: "red flush original invoice no longer issued",
			steps: append(retryDeadLetterBegin(), deadOutbox("DEAD_LETTER", "SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED", "invoice-1"),
				scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_red_flush_attempt", Rows: [][]driver.Value{{"PENDING", true, "red-request-1", "event-1", int64(1), "ISSUE_PENDING", "invoice-1", "RED_FLUSHED"}}}),
			want: ErrConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rollback bool
			steps := tc.steps
			steps = append(steps, scriptdb.Step{Kind: "rollback", After: func() { rollback = true }})
			s := &Service{DB: scriptdb.Open(t, steps...)}
			err := s.RetryTaxCommandDeadLetter(context.Background(), taxDeadLetterPrincipal(), "key-1", "outbox-1", TaxCommandDeadLetterRetryInput{Reason: "核实税控命令死信"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
			if !rollback {
				t.Fatal("rejected retry must roll back without changing command state")
			}
		})
	}
}

func aggregateTypeFor(eventType string) string {
	if eventType == "SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED" {
		return "tax_invoice"
	}
	return "invoice_request"
}

// TestRetryTaxCommandDeadLetterResetsSameCommandAndAudits 验证成功重试只把同一命令
// 重置为待投递，不新建命令、不释放额度，并写入高风险审计事实。
// 参数：t 测试上下文。
// 返回值：无；状态更新条件、载荷或审计缺失时失败。
func TestRetryTaxCommandDeadLetterResetsSameCommandAndAudits(t *testing.T) {
	for _, eventType := range []string{"SETTLEMENT_INVOICE_ISSUE_REQUESTED", "SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED"} {
		t.Run(eventType, func(t *testing.T) {
			payload := []byte(`{"attempt_id":"attempt-1"}`)
			attemptStep := scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_issue_attempt", Rows: [][]driver.Value{{"PENDING", true, "request-1", "event-1", int64(1), "ISSUE_PENDING"}}}
			if eventType == "SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED" {
				payload = []byte(`{"red_flush_attempt_id":"red-attempt-1"}`)
				attemptStep = scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_red_flush_attempt", Rows: [][]driver.Value{{"PENDING", true, "red-request-1", "event-1", int64(1), "ISSUE_PENDING", "invoice-1", "ISSUED"}}}
			}
			var updateArgs, auditArgs []driver.NamedValue
			steps := append(retryDeadLetterBegin(),
				scriptdb.Step{Kind: "query", SQL: "FROM settlement_outbox_event", Rows: [][]driver.Value{{"TAX_INVOICE_COMMAND", "DEAD_LETTER", eventType, "event-1", aggregateTypeFor(eventType), payload, "FAILED", "终态失败"}}},
				attemptStep,
				scriptdb.Step{Kind: "query", SQL: "newer.attempt_no>?", Rows: [][]driver.Value{{true}}},
				scriptdb.Step{Kind: "exec", SQL: "SET status='PENDING',attempt_count=0", Args: &updateArgs},
				scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: &auditArgs},
				scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
				scriptdb.Step{Kind: "commit"},
			)
			s := &Service{DB: scriptdb.Open(t, steps...)}
			if err := s.RetryTaxCommandDeadLetter(context.Background(), taxDeadLetterPrincipal(), "key-1", "outbox-1", TaxCommandDeadLetterRetryInput{Reason: "核实税控命令死信"}); err != nil {
				t.Fatal(err)
			}
			if len(updateArgs) == 0 || updateArgs[0].Value != "tenant-a" || updateArgs[1].Value != "outbox-1" {
				t.Fatalf("update must target the same tenant command: %+v", updateArgs)
			}
			// outbox 行参数顺序：id,tenant,event_id,destination,event_type,aggregate_type,aggregate_id,payload。
			if len(auditArgs) < 8 || auditArgs[3].Value != "PLATFORM_AUDIT" || auditArgs[4].Value != "SETTLEMENT_TAX_COMMAND_DEAD_LETTER_RETRIED" || auditArgs[5].Value != "outbox_event" || auditArgs[6].Value != "outbox-1" {
				t.Fatalf("audit event must bind retry actor and command: %+v", auditArgs)
			}
			body, ok := auditArgs[7].Value.([]byte)
			if !ok || !strings.Contains(string(body), `"reason":"核实税控命令死信"`) || !strings.Contains(string(body), `"previous_error_code":"FAILED"`) || !strings.Contains(string(body), `"risk_level":"HIGH"`) {
				t.Fatalf("audit payload must preserve reason and previous failure: %s", body)
			}
			if strings.Contains(string(body), "released") || strings.Contains(string(body), "RELEASED") {
				t.Fatal("retry must not release quota")
			}
		})
	}
}

// TestRetryTaxCommandDeadLetterReplayIsIdempotent 验证同幂等键重放返回首次结果，
// 不重复改状态或写审计；不同键的重复操作仍被状态机拒绝。
// 参数：t 测试上下文。
// 返回值：无；重放写入或哈希不一致未被拒绝时失败。
func TestRetryTaxCommandDeadLetterReplayIsIdempotent(t *testing.T) {
	reason := "核实税控命令死信"
	body, _ := json.Marshal(map[string]any{"actor_id": "finance-1", "id": "outbox-1", "reason": reason})
	hash := sha256.Sum256(body)
	var committed bool
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: errors.New("Duplicate entry")},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{hash[:], "outbox-1"}}},
		scriptdb.Step{Kind: "commit", After: func() { committed = true }},
	)
	if err := (&Service{DB: db}).RetryTaxCommandDeadLetter(context.Background(), taxDeadLetterPrincipal(), "key-1", "outbox-1", TaxCommandDeadLetterRetryInput{Reason: reason}); err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("replay must commit without mutating the command")
	}

	conflict := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: errors.New("Duplicate entry")},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{[]byte("different-hash"), "outbox-1"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	if err := (&Service{DB: conflict}).RetryTaxCommandDeadLetter(context.Background(), taxDeadLetterPrincipal(), "key-1", "outbox-1", TaxCommandDeadLetterRetryInput{Reason: reason}); !errors.Is(err, ErrConflict) {
		t.Fatalf("hash mismatch err=%v want ErrConflict", err)
	}
}
