package service

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// invoiceCancelScenario 描述一次作废状态机场景所需的既有事实。
type invoiceCancelScenario struct {
	name                       string
	requestStatus, applicant   string
	attemptStatus, lockedCmd   string
	commandEvent, outboxStatus string
	withAttempt                bool
	neverAccepted              bool
	invoices, unresolved       int64
	storedVersion, wantVersion int
	actor                      string
	want                       error
}

// invoiceCancelPrincipal 构造具备发票执行权限的作废主体。
// 参数：user为执行人。
// 返回值：带 tenant/user 的可信主体。
func invoiceCancelPrincipal(user string) Principal {
	return Principal{TenantID: "tenant-a", UserID: user, Permissions: map[string]bool{"settlement.invoice.issue": true}}
}

// invoiceCancelSteps 按服务真实调用顺序构造脚本步骤；拒绝场景只到失败点为止。
// 参数：scenario为场景，auditArgs为捕获审计参数的出口。
// 返回值：脚本步骤列表。
func invoiceCancelSteps(scenario invoiceCancelScenario, auditArgs *[]driver.NamedValue) []scriptdb.Step {
	steps := []scriptdb.Step{
		{Kind: "begin"},
		{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
	}
	if scenario.withAttempt {
		steps = append(steps, scriptdb.Step{Kind: "query", SQL: "SELECT id,COALESCE(command_event_id,'') FROM settlement_invoice_issue_attempt", Rows: [][]driver.Value{{"attempt-1", scenario.commandEvent}}})
	} else {
		steps = append(steps, scriptdb.Step{Kind: "query", SQL: "SELECT id,COALESCE(command_event_id,'') FROM settlement_invoice_issue_attempt"})
	}
	if scenario.withAttempt && scenario.commandEvent != "" {
		if scenario.outboxStatus == "" {
			steps = append(steps, scriptdb.Step{Kind: "query", SQL: "settlement_outbox_event WHERE tenant_id=? AND event_id=? ORDER BY created_at DESC,id DESC LIMIT 1 FOR UPDATE"})
			return append(steps, scriptdb.Step{Kind: "rollback"})
		}
		steps = append(steps, scriptdb.Step{Kind: "query", SQL: "settlement_outbox_event WHERE tenant_id=? AND event_id=? ORDER BY created_at DESC,id DESC LIMIT 1 FOR UPDATE", Rows: [][]driver.Value{{"TAX_INVOICE_COMMAND", scenario.outboxStatus}}})
		if scenario.outboxStatus != "DEAD_LETTER" {
			return append(steps, scriptdb.Step{Kind: "rollback"})
		}
	}
	if scenario.withAttempt {
		steps = append(steps, scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_issue_attempt WHERE tenant_id=? AND id=? FOR UPDATE", Rows: [][]driver.Value{{scenario.attemptStatus, scenario.neverAccepted, scenario.lockedCmd}}})
		if scenario.lockedCmd != scenario.commandEvent {
			return append(steps, scriptdb.Step{Kind: "rollback"})
		}
		if !scenario.neverAccepted || (scenario.attemptStatus != "PENDING" && scenario.attemptStatus != "ISSUE_FAILED") {
			return append(steps, scriptdb.Step{Kind: "rollback"})
		}
	}
	steps = append(steps, scriptdb.Step{Kind: "query", SQL: "FROM settlement_invoice_request WHERE id=? AND tenant_id=? FOR UPDATE", Rows: [][]driver.Value{{scenario.requestStatus, scenario.applicant, int64(scenario.storedVersion)}}})
	if (scenario.requestStatus != "ISSUE_PENDING" && scenario.requestStatus != "ISSUE_FAILED") || scenario.storedVersion != scenario.wantVersion {
		return append(steps, scriptdb.Step{Kind: "rollback"})
	}
	if scenario.applicant == scenario.actor {
		return append(steps, scriptdb.Step{Kind: "rollback"})
	}
	steps = append(steps, scriptdb.Step{Kind: "query", SQL: "SELECT COUNT(*) FROM settlement_tax_invoice", Rows: [][]driver.Value{{scenario.invoices}}})
	if scenario.invoices != 0 {
		return append(steps, scriptdb.Step{Kind: "rollback"})
	}
	steps = append(steps, scriptdb.Step{Kind: "query", SQL: "SELECT COUNT(*) FROM settlement_tax_result_inbox", Rows: [][]driver.Value{{scenario.unresolved}}})
	if scenario.unresolved != 0 {
		return append(steps, scriptdb.Step{Kind: "rollback"})
	}
	if scenario.withAttempt {
		steps = append(steps, scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_issue_attempt SET status='ABANDONED'"})
	}
	steps = append(steps,
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_request SET status='CANCELLED'"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_request_allocation SET released_amount=reserved_amount-invoiced_amount"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: auditArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "commit"},
	)
	return steps
}

// TestCancelInvoiceRequestStateMatrix 验证作废只放行"确定未受理"的场景：
// 已受理、未知、厂商冲突、已开票、重投中、命令缺失、申请人自审全部失败关闭。
// 参数：t为测试上下文。
// 返回值：无；任一场景状态机或写入边界不符时失败。
func TestCancelInvoiceRequestStateMatrix(t *testing.T) {
	for _, scenario := range []invoiceCancelScenario{
		{name: "死信后作废释放额度", requestStatus: "ISSUE_PENDING", applicant: "applicant", attemptStatus: "PENDING", neverAccepted: true, commandEvent: "event-1", outboxStatus: "DEAD_LETTER", lockedCmd: "event-1", withAttempt: true, storedVersion: 3, wantVersion: 3, actor: "reviewer"},
		{name: "无命令的人工申请作废", requestStatus: "ISSUE_PENDING", applicant: "applicant", storedVersion: 2, wantVersion: 2, actor: "reviewer"},
		{name: "已明确失败的申请作废", requestStatus: "ISSUE_FAILED", applicant: "applicant", attemptStatus: "ISSUE_FAILED", neverAccepted: true, commandEvent: "event-1", outboxStatus: "DEAD_LETTER", lockedCmd: "event-1", withAttempt: true, storedVersion: 3, wantVersion: 3, actor: "reviewer"},
		{name: "税控已受理必须拒绝", requestStatus: "ISSUE_PENDING", applicant: "applicant", attemptStatus: "PENDING", neverAccepted: false, commandEvent: "event-1", outboxStatus: "DEAD_LETTER", lockedCmd: "event-1", withAttempt: true, storedVersion: 3, wantVersion: 3, actor: "reviewer", want: ErrConflict},
		{name: "未知结果必须拒绝", requestStatus: "ISSUE_UNKNOWN", applicant: "applicant", attemptStatus: "UNKNOWN", neverAccepted: true, commandEvent: "event-1", outboxStatus: "DEAD_LETTER", lockedCmd: "event-1", withAttempt: true, storedVersion: 3, wantVersion: 3, actor: "reviewer", want: ErrConflict},
		{name: "厂商冲突结果必须拒绝", requestStatus: "ISSUE_PENDING", applicant: "applicant", attemptStatus: "PENDING", neverAccepted: true, commandEvent: "event-1", outboxStatus: "DEAD_LETTER", lockedCmd: "event-1", withAttempt: true, unresolved: 1, storedVersion: 3, wantVersion: 3, actor: "reviewer", want: ErrConflict},
		{name: "已有发票必须拒绝", requestStatus: "ISSUE_PENDING", applicant: "applicant", attemptStatus: "PENDING", neverAccepted: true, commandEvent: "event-1", outboxStatus: "DEAD_LETTER", lockedCmd: "event-1", withAttempt: true, invoices: 1, storedVersion: 3, wantVersion: 3, actor: "reviewer", want: ErrConflict},
		{name: "命令已被重投必须拒绝", requestStatus: "ISSUE_PENDING", applicant: "applicant", attemptStatus: "PENDING", neverAccepted: true, commandEvent: "event-1", outboxStatus: "PENDING", lockedCmd: "event-1", withAttempt: true, storedVersion: 3, wantVersion: 3, actor: "reviewer", want: ErrConflict},
		{name: "命令事实缺失必须拒绝", requestStatus: "ISSUE_PENDING", applicant: "applicant", attemptStatus: "PENDING", neverAccepted: true, commandEvent: "event-1", outboxStatus: "", lockedCmd: "event-1", withAttempt: true, storedVersion: 3, wantVersion: 3, actor: "reviewer", want: ErrConflict},
		{name: "命令已被新尝试取代必须拒绝", requestStatus: "ISSUE_PENDING", applicant: "applicant", attemptStatus: "PENDING", neverAccepted: true, commandEvent: "event-1", outboxStatus: "DEAD_LETTER", lockedCmd: "event-2", withAttempt: true, storedVersion: 3, wantVersion: 3, actor: "reviewer", want: ErrConflict},
		{name: "版本不一致必须拒绝", requestStatus: "ISSUE_PENDING", applicant: "applicant", attemptStatus: "PENDING", neverAccepted: true, commandEvent: "event-1", outboxStatus: "DEAD_LETTER", lockedCmd: "event-1", withAttempt: true, storedVersion: 4, wantVersion: 3, actor: "reviewer", want: ErrConflict},
		{name: "申请人必须回避", requestStatus: "ISSUE_PENDING", applicant: "reviewer", attemptStatus: "PENDING", neverAccepted: true, commandEvent: "event-1", outboxStatus: "DEAD_LETTER", lockedCmd: "event-1", withAttempt: true, storedVersion: 3, wantVersion: 3, actor: "reviewer", want: ErrInvalid},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var auditArgs []driver.NamedValue
			db := scriptdb.Open(t, invoiceCancelSteps(scenario, &auditArgs)...)
			svc := &Service{DB: db}
			err := svc.CancelInvoiceRequest(context.Background(), invoiceCancelPrincipal(scenario.actor), "cancel-key-1", "request-1", scenario.wantVersion, "税控命令死信后放弃开票")
			if !errors.Is(err, scenario.want) {
				t.Fatalf("error=%v want=%v", err, scenario.want)
			}
			if scenario.want != nil {
				if len(auditArgs) != 0 {
					t.Fatalf("拒绝场景不得写审计: %+v", auditArgs)
				}
				return
			}
			if len(auditArgs) < 8 {
				t.Fatalf("缺少作废审计: %+v", auditArgs)
			}
			if eventType, _ := auditArgs[4].Value.(string); eventType != "SETTLEMENT_INVOICE_CANCELLED" {
				t.Fatalf("审计事件类型=%v", auditArgs[4].Value)
			}
			payload, _ := auditArgs[7].Value.([]byte)
			for _, want := range []string{`"risk_level":"HIGH"`, `"reason":"税控命令死信后放弃开票"`, `"attempt_id":"`, `"previous_status":"`} {
				if !strings.Contains(string(payload), want) {
					t.Fatalf("作废审计载荷缺少 %s: %s", want, payload)
				}
			}
		})
	}
}

// TestCancelInvoiceRequestIdempotentReplay 验证相同幂等键重放不重复改状态、
// 释放额度或写审计，异内容重放返回冲突。
// 参数：t为测试上下文。
// 返回值：无；重放语义不符时失败。
func TestCancelInvoiceRequestIdempotentReplay(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"actor_id": "reviewer", "id": "request-1", "version": 3, "reason": "税控命令死信后放弃开票"})
	hash := sha256.Sum256(body)
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{hash[:], "request-1"}}},
		scriptdb.Step{Kind: "commit"},
	)
	svc := &Service{DB: db}
	if err := svc.CancelInvoiceRequest(context.Background(), invoiceCancelPrincipal("reviewer"), "cancel-key-1", "request-1", 3, "税控命令死信后放弃开票"); err != nil {
		t.Fatalf("幂等重放应成功: %v", err)
	}
	mismatch := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{[]byte("different-hash"), "request-1"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	if err := (&Service{DB: mismatch}).CancelInvoiceRequest(context.Background(), invoiceCancelPrincipal("reviewer"), "cancel-key-1", "request-1", 3, "税控命令死信后放弃开票"); !errors.Is(err, ErrConflict) {
		t.Fatalf("异内容重放必须冲突: %v", err)
	}
}

// TestCancelInvoiceRequestValidatesBeforeDatabase 验证参数不合法时不进入事务。
// 参数：t为测试上下文。
// 返回值：无；未在进库前拒绝时失败。
func TestCancelInvoiceRequestValidatesBeforeDatabase(t *testing.T) {
	svc := &Service{}
	for _, in := range []struct {
		key     string
		version int
		reason  string
		actor   string
	}{
		{key: "", version: 1, reason: "税控命令死信后放弃开票", actor: "reviewer"},
		{key: "k", version: 0, reason: "税控命令死信后放弃开票", actor: "reviewer"},
		{key: "k", version: 1, reason: "短", actor: "reviewer"},
		{key: "k", version: 1, reason: "税控命令死信后放弃开票"},
	} {
		if err := svc.CancelInvoiceRequest(context.Background(), invoiceCancelPrincipal(in.actor), in.key, "request-1", in.version, in.reason); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrForbidden) {
			t.Fatalf("非法参数必须拒绝: %v", err)
		}
	}
	unauthorized := Principal{TenantID: "tenant-a", UserID: "reviewer", Permissions: map[string]bool{"settlement.invoice.read": true}}
	if err := svc.CancelInvoiceRequest(context.Background(), unauthorized, "k", "request-1", 1, "税控命令死信后放弃开票"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("缺少settlement.invoice.issue必须禁止: %v", err)
	}
}

// TestCancelInvoiceRequestLosesRaceToRetry 模拟"重投×作废"交错：重投先把命令
// 重置为待投递，作废在行锁复核阶段看到 PENDING 后失败关闭，不释放额度。
// 参数：t为测试上下文。
// 返回值：无；竞争失败方未失败关闭时失败。
func TestCancelInvoiceRequestLosesRaceToRetry(t *testing.T) {
	scenario := invoiceCancelScenario{name: "并发重投后作废失败", requestStatus: "ISSUE_PENDING", applicant: "applicant", attemptStatus: "PENDING", neverAccepted: true, commandEvent: "event-1", outboxStatus: "PENDING", lockedCmd: "event-1", withAttempt: true, storedVersion: 3, wantVersion: 3, actor: "reviewer", want: ErrConflict}
	var auditArgs []driver.NamedValue
	db := scriptdb.Open(t, invoiceCancelSteps(scenario, &auditArgs)...)
	if err := (&Service{DB: db}).CancelInvoiceRequest(context.Background(), invoiceCancelPrincipal("reviewer"), "cancel-key-1", "request-1", 3, "税控命令死信后放弃开票"); !errors.Is(err, ErrConflict) {
		t.Fatalf("重投中的命令不得作废: %v", err)
	}
	if len(auditArgs) != 0 {
		t.Fatalf("失败方不得写审计: %+v", auditArgs)
	}
}
