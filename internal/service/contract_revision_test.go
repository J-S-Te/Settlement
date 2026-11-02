package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// revisionContractEvent 构造稳定的财务修订事件，避免测试依赖真实合同服务。
// 参数：eventID 为事件标识，version 为合同版本，amount 为合同金额，terms 为付款计划。
// 返回值：填充完整身份字段的合同事件。
func revisionContractEvent(eventID string, version int, amount string, terms ...PaymentTerm) ContractEvent {
	event := ContractEvent{EventID: eventID, EventType: "contract.financial_effective.v1", TenantID: "tenant-a", OccurredAt: time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)}
	event.Contract.ID = "contract-1"
	event.Contract.No = "HT-1"
	event.Contract.Version = version
	event.Contract.CustomerID = "customer-1"
	event.Contract.CustomerName = "隔离客户"
	event.Contract.Currency = "CNY"
	event.Contract.Amount = amount
	event.Contract.PaymentTerms = terms
	return event
}

// revisionTerm 构造单个付款计划。
// 参数：no 为分期号，due 为到期日，amount 为金额。
// 返回值：付款计划。
func revisionTerm(no int, due, amount string) PaymentTerm {
	return PaymentTerm{InstallmentNo: no, DueDate: due, Amount: amount}
}

// TestIngestContractFirstVersionKeepsBaseline 验证首版基线语义不变：写入快照与待确认计划。
// 参数：t 为测试上下文。
// 返回值：无；结果或 SQL 边界不符时失败。
func TestIngestContractFirstVersionKeepsBaseline(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_stream"},
		scriptdb.Step{Kind: "query", SQL: "SELECT current_version FROM settlement_contract_stream", Rows: [][]driver.Value{{int64(0)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_snapshot"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_receivable_plan"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_receivable_plan"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_stream"},
		scriptdb.Step{Kind: "commit"},
	)
	svc := &Service{DB: db}
	result, err := svc.IngestContract(context.Background(), "contract_management", "tenant-a", revisionContractEvent("EVT-1", 1, "100.00", revisionTerm(1, "2099-01-01", "50.00"), revisionTerm(2, "2099-02-01", "50.00")))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.RevisionHeld || result.HoldID != "" {
		t.Fatalf("首版结果=%+v", result)
	}
}

// TestIngestContractRevisionRearrangesPendingPlans 验证升版时待确认计划直接重排，
// 旧快照降级为 SUPERSEDED，新快照生效并推进合同版本。
// 参数：t 为测试上下文。
// 返回值：无；快照、计划或结果不符时失败。
func TestIngestContractRevisionRearrangesPendingPlans(t *testing.T) {
	var snapshotArgs, supersedeArgs, firstPlanArgs, secondPlanArgs, streamArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_stream"},
		scriptdb.Step{Kind: "query", SQL: "SELECT current_version FROM settlement_contract_stream", Rows: [][]driver.Value{{int64(2)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_inbox_event"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id,contract_amount,migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"snapshot-old", "100.00", "RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan rp LEFT JOIN settlement_receivable r", Rows: [][]driver.Value{
			{"plan-1", int64(1), "2099-01-01", "50.00", "PENDING_CONFIRMATION", "", "0", "0", "0", "0", int64(0)},
			{"plan-2", int64(2), "2099-02-01", "50.00", "PENDING_CONFIRMATION", "", "0", "0", "0", "0", int64(0)},
		}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_snapshot", Args: &snapshotArgs},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_snapshot SET financial_status='SUPERSEDED'", Args: &supersedeArgs},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable_plan SET contract_snapshot_id=?,due_date=?", Args: &firstPlanArgs},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable_plan SET contract_snapshot_id=?,due_date=?", Args: &secondPlanArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_stream", Args: &streamArgs},
		scriptdb.Step{Kind: "commit"},
	)
	svc := &Service{DB: db}
	result, err := svc.IngestContract(context.Background(), "contract_management", "tenant-a", revisionContractEvent("EVT-2", 3, "100.00", revisionTerm(1, "2099-03-01", "40.00"), revisionTerm(2, "2099-04-01", "60.00")))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.RevisionHeld {
		t.Fatalf("升版结果=%+v", result)
	}
	// 新快照行必须记录新版本、100 元合同金额和 EFFECTIVE 状态。
	if len(snapshotArgs) < 14 || fmt.Sprint(snapshotArgs[6].Value) != "3" || snapshotArgs[12].Value != "100.00" {
		t.Fatalf("新快照绑定参数=%+v", snapshotArgs)
	}
	if len(supersedeArgs) != 4 || supersedeArgs[3].Value != int64(2) {
		t.Fatalf("旧快照降级参数=%+v", supersedeArgs)
	}
	if len(firstPlanArgs) != 5 || firstPlanArgs[1].Value != "2099-03-01" || firstPlanArgs[2].Value != "40.00" {
		t.Fatalf("第一期重排参数=%+v", firstPlanArgs)
	}
	if len(secondPlanArgs) != 5 || secondPlanArgs[1].Value != "2099-04-01" || secondPlanArgs[2].Value != "60.00" {
		t.Fatalf("第二期重排参数=%+v", secondPlanArgs)
	}
	if len(streamArgs) != 4 || fmt.Sprint(streamArgs[0].Value) != "3" || streamArgs[3].Value != "contract-1" {
		t.Fatalf("合同版本推进参数=%+v", streamArgs)
	}
}

// TestIngestContractRevisionAdjustsConfirmedPlanWithoutFacts 验证已确认但无任何
// 已实现事实的计划允许改额，并同步应收与 SETTLEMENT_PLAN_ADJUSTED 审计。
// 参数：t 为测试上下文。
// 返回值：无；应收同步或审计缺失时失败。
func TestIngestContractRevisionAdjustsConfirmedPlanWithoutFacts(t *testing.T) {
	var planArgs, receivableArgs, auditArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_stream"},
		scriptdb.Step{Kind: "query", SQL: "SELECT current_version FROM settlement_contract_stream", Rows: [][]driver.Value{{int64(1)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_inbox_event"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id,contract_amount,migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"snapshot-old", "100.00", "RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan rp LEFT JOIN settlement_receivable r", Rows: [][]driver.Value{
			{"plan-1", int64(1), "2099-01-01", "50.00", "CONFIRMED", "receivable-1", "0", "0", "0", "0", int64(0)},
		}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_snapshot"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_snapshot SET financial_status='SUPERSEDED'"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable_plan SET contract_snapshot_id=?,due_date=?", Args: &planArgs},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable SET contract_snapshot_id=?,due_date=?", Args: &receivableArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: &auditArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_receivable_plan"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_stream"},
		scriptdb.Step{Kind: "commit"},
	)
	svc := &Service{DB: db}
	result, err := svc.IngestContract(context.Background(), "contract_management", "tenant-a", revisionContractEvent("EVT-3", 2, "100.00", revisionTerm(1, "2099-05-01", "80.00"), revisionTerm(2, "2099-06-01", "20.00")))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.RevisionHeld {
		t.Fatalf("已确认无事实改额结果=%+v", result)
	}
	if len(planArgs) != 5 || planArgs[1].Value != "2099-05-01" || planArgs[2].Value != "80.00" {
		t.Fatalf("计划改额参数=%+v", planArgs)
	}
	if len(receivableArgs) != 7 || receivableArgs[1].Value != "2099-05-01" || receivableArgs[2].Value != "80.00" {
		t.Fatalf("应收同步参数=%+v", receivableArgs)
	}
	assertRevisionAudit(t, auditArgs, "SETTLEMENT_PLAN_ADJUSTED", `"action":"ADJUSTED"`, `"amount":"80.00"`)
}

// TestIngestContractRevisionHoldsImplementedFacts 验证含回款/开票/预占事实的差异
// 不自动改额，计划保持原值并登记人工复核工单；未实现分期仍自动重排。
// 参数：t 为测试上下文。
// 返回值：无；工单或计划处理不符合 fail-closed 时失败。
func TestIngestContractRevisionHoldsImplementedFacts(t *testing.T) {
	var holdArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_stream"},
		scriptdb.Step{Kind: "query", SQL: "SELECT current_version FROM settlement_contract_stream", Rows: [][]driver.Value{{int64(1)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_inbox_event"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id,contract_amount,migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"snapshot-old", "100.00", "RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan rp LEFT JOIN settlement_receivable r", Rows: [][]driver.Value{
			{"plan-1", int64(1), "2099-01-01", "50.00", "CONFIRMED", "receivable-1", "20.00", "0", "10.00", "5.00", int64(0)},
			{"plan-2", int64(2), "2099-02-01", "50.00", "PENDING_CONFIRMATION", "", "0", "0", "0", "0", int64(0)},
		}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_snapshot"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_snapshot SET financial_status='SUPERSEDED'"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable_plan SET contract_snapshot_id=?,updated_at"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable SET contract_snapshot_id=?,updated_at"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable_plan SET contract_snapshot_id=?,due_date=?"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_revision_hold", Args: &holdArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_stream"},
		scriptdb.Step{Kind: "commit"},
	)
	svc := &Service{DB: db}
	result, err := svc.IngestContract(context.Background(), "contract_management", "tenant-a", revisionContractEvent("EVT-4", 2, "100.00", revisionTerm(1, "2099-03-01", "60.00"), revisionTerm(2, "2099-04-01", "40.00")))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || !result.RevisionHeld || result.HoldID == "" {
		t.Fatalf("含已实现事实结果=%+v", result)
	}
	if len(holdArgs) < 11 || holdArgs[9].Value != contractRevisionHoldPlanDiff {
		t.Fatalf("工单参数=%+v", holdArgs)
	}
	body, ok := holdArgs[10].Value.([]byte)
	if !ok {
		t.Fatalf("工单差异载荷类型=%T", holdArgs[10].Value)
	}
	for _, want := range []string{`"kind":"AMOUNT_AND_DUE_DATE_CHANGED"`, `"old_amount":"50.00"`, `"new_amount":"60.00"`, `"received_allocated":"20.00"`, `"invoiced":"10.00"`, `"reserved":"5.00"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("工单差异缺少 %s: %s", want, body)
		}
	}
}

// TestIngestContractRevisionHoldsUnbalancedTerms 验证新条款合计不等于新合同金额时
// 整次修订 fail-closed：只登记工单，不写快照、不推进版本、不改计划。
// 参数：t 为测试上下文。
// 返回值：无；出现任何自动应用时失败。
func TestIngestContractRevisionHoldsUnbalancedTerms(t *testing.T) {
	var holdArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_stream"},
		scriptdb.Step{Kind: "query", SQL: "SELECT current_version FROM settlement_contract_stream", Rows: [][]driver.Value{{int64(1)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_inbox_event"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id,contract_amount,migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"snapshot-old", "100.00", "RELEASED"}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_revision_hold", Args: &holdArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_inbox_event"},
		scriptdb.Step{Kind: "commit"},
	)
	svc := &Service{DB: db}
	result, err := svc.IngestContract(context.Background(), "contract_management", "tenant-a", revisionContractEvent("EVT-5", 2, "100.00", revisionTerm(1, "2099-03-01", "40.00"), revisionTerm(2, "2099-04-01", "40.00")))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || !result.RevisionHeld || result.HoldID == "" {
		t.Fatalf("条款不平结果=%+v", result)
	}
	if len(holdArgs) < 11 || holdArgs[9].Value != contractRevisionHoldTermTotal {
		t.Fatalf("条款不平行工单参数=%+v", holdArgs)
	}
	body, ok := holdArgs[10].Value.([]byte)
	if !ok || !strings.Contains(string(body), `"new_term_total":"80.00"`) || !strings.Contains(string(body), `"new_contract_amount":"100.00"`) {
		t.Fatalf("条款不平差异载荷=%s", body)
	}
}

// TestIngestContractRejectsVersionRegression 验证版本回退不自动应用，保持冲突语义。
// 参数：t 为测试上下文。
// 返回值：无；回退未被拒绝或发生了写入时失败。
func TestIngestContractRejectsVersionRegression(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_stream"},
		scriptdb.Step{Kind: "query", SQL: "SELECT current_version FROM settlement_contract_stream", Rows: [][]driver.Value{{int64(2)}}},
		scriptdb.Step{Kind: "rollback"},
	)
	svc := &Service{DB: db}
	_, err := svc.IngestContract(context.Background(), "contract_management", "tenant-a", revisionContractEvent("EVT-6", 1, "100.00", revisionTerm(1, "2099-01-01", "100.00")))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("版本回退错误=%v", err)
	}
}

// TestIngestContractRevisionReplayIsIdempotent 验证同一 event_id 重放返回成功重复，
// 不重复登记工单、不重复改计划。
// 参数：t 为测试上下文。
// 返回值：无；重放被当作新事件时失败。
func TestIngestContractRevisionReplayIsIdempotent(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_inbox_event", Rows: [][]driver.Value{{"inbox-1"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	svc := &Service{DB: db}
	result, err := svc.IngestContract(context.Background(), "contract_management", "tenant-a", revisionContractEvent("EVT-7", 2, "100.00", revisionTerm(1, "2099-01-01", "100.00")))
	if err != nil {
		t.Fatal(err)
	}
	if result.Created || result.RevisionHeld || result.HoldID != "" {
		t.Fatalf("重放结果=%+v", result)
	}
}

// TestListContractRevisionHoldsBindsTenantAndScope 验证工单列表按租户与责任范围查询，
// 并返回可直接核对差异的明细。
// 参数：t 为测试上下文。
// 返回值：无；查询绑定或响应字段不符时失败。
func TestListContractRevisionHoldsBindsTenantAndScope(t *testing.T) {
	var args []driver.NamedValue
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_revision_hold cs", Args: &args, Rows: [][]driver.Value{
		{"hold-1", "contract-1", "HT-1", "snapshot-new", "EVT-4", int64(1), int64(2), contractRevisionHoldPlanDiff, []byte(`{"diffs":[{"installment_no":1}]}`), "PENDING_REVIEW", "", "", "2026-10-30T08:00:00.000000Z"},
	}})
	svc := &Service{DB: db}
	p := Principal{TenantID: "tenant-a", UserID: "finance", Permissions: map[string]bool{"settlement.revision.read": true}}
	items, err := svc.ListContractRevisionHolds(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "hold-1" || items[0].Reason != contractRevisionHoldPlanDiff || items[0].ToVersion != 2 || !strings.Contains(string(items[0].Diff), `"installment_no":1`) {
		t.Fatalf("工单列表=%+v", items)
	}
	if len(args) == 0 || args[0].Value != "tenant-a" {
		t.Fatalf("租户绑定参数=%+v", args)
	}
}

// TestResolveContractRevisionHoldWritesAudit 验证财务处理完成后工单置为 RESOLVED 并留高风险审计。
// 参数：t 为测试上下文。
// 返回值：无；状态或审计缺失时失败。
func TestResolveContractRevisionHoldWritesAudit(t *testing.T) {
	var auditArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT status,COALESCE(contract_snapshot_id,'') FROM settlement_contract_revision_hold", Rows: [][]driver.Value{{"PENDING_REVIEW", "snapshot-new"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_revision_hold SET status='RESOLVED'"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: &auditArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "commit"},
	)
	svc := &Service{DB: db}
	p := Principal{TenantID: "tenant-a", UserID: "finance-manager", Permissions: map[string]bool{"settlement.revision.resolve": true}}
	if err := svc.ResolveContractRevisionHold(context.Background(), p, "hold-1", "已按修订金额完成调整与红冲"); err != nil {
		t.Fatal(err)
	}
	assertRevisionAudit(t, auditArgs, "SETTLEMENT_CONTRACT_REVISION_HOLD_RESOLVED", `"risk_level":"HIGH"`)
}

// TestResolveContractRevisionHoldRequiresDedicatedPermission 验证关单只认专用动作
// 权限：旧的应收调整权限不再放行，缺少专用权限在进入事务前即被拒绝。
// 参数：t 为测试上下文。
// 返回值：无；旧权限仍可关单或专用权限被误拒时失败。
func TestResolveContractRevisionHoldRequiresDedicatedPermission(t *testing.T) {
	svc := &Service{}
	legacyAdjust := Principal{TenantID: "tenant-a", UserID: "sales", Permissions: map[string]bool{"settlement.receivable.adjust": true}}
	if err := svc.ResolveContractRevisionHold(context.Background(), legacyAdjust, "hold-1", "已按修订金额完成调整与红冲"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("仅 settlement.receivable.adjust 不得关单: %v", err)
	}
	readOnly := Principal{TenantID: "tenant-a", UserID: "assistant", Permissions: map[string]bool{"settlement.revision.read": true}}
	if err := svc.ResolveContractRevisionHold(context.Background(), readOnly, "hold-1", "已按修订金额完成调整与红冲"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("只读权限不得关单: %v", err)
	}
}

// TestResolveContractRevisionHoldValidatesRuneLength 验证处理说明按字符（rune）5–500
// 校验，与前端“5–500 字”口径一致；按字节判会放过 2 个汉字。
// 参数：t 为测试上下文。
// 返回值：无；4 字、空值或 501 字未被拒绝时失败。
func TestResolveContractRevisionHoldValidatesRuneLength(t *testing.T) {
	svc := &Service{}
	p := Principal{TenantID: "tenant-a", UserID: "finance-manager", Permissions: map[string]bool{"settlement.revision.resolve": true}}
	// “已按修a”为 3 个汉字 + 1 个字母 = 4 字、10 字节；旧字节下限会误放行。
	for _, reason := range []string{"", "   ", "短", "已按修a", strings.Repeat("字", 501)} {
		if err := svc.ResolveContractRevisionHold(context.Background(), p, "hold-1", reason); !errors.Is(err, ErrInvalid) {
			t.Fatalf("reason=%q 必须拒绝: %v", reason, err)
		}
	}
	if err := svc.ResolveContractRevisionHold(context.Background(), p, "", "已按修订金额完成调整与红冲"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("缺少工单ID必须拒绝: %v", err)
	}
}

// assertRevisionAudit 断言审计事件类型与载荷关键字段。
// 参数：t 为测试上下文，args 为捕获的审计参数，eventType 为期望事件类型，
// wants 为载荷中必须出现的片段。
// 返回值：无；审计缺失或载荷不符时失败。
func assertRevisionAudit(t *testing.T, args []driver.NamedValue, eventType string, wants ...string) {
	t.Helper()
	if len(args) < 8 {
		t.Fatalf("审计参数不足: %+v", args)
	}
	if args[4].Value != eventType {
		t.Fatalf("审计事件类型=%v，期望 %s", args[4].Value, eventType)
	}
	payload, ok := args[7].Value.([]byte)
	if !ok {
		t.Fatalf("审计载荷类型=%T", args[7].Value)
	}
	for _, want := range wants {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("审计载荷缺少 %s: %s", want, payload)
		}
	}
}

// TestIngestContractRevisionRemovesPendingInstallment 验证新条款删除分期时，
// 无已实现事实且无外部引用的待确认计划被删建并写 SETTLEMENT_PLAN_ADJUSTED 审计。
// 参数：t 为测试上下文。
// 返回值：无；删除未执行或审计缺失时失败。
func TestIngestContractRevisionRemovesPendingInstallment(t *testing.T) {
	var removeAudit []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_stream"},
		scriptdb.Step{Kind: "query", SQL: "SELECT current_version FROM settlement_contract_stream", Rows: [][]driver.Value{{int64(1)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_inbox_event"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id,contract_amount,migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"snapshot-old", "100.00", "RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan rp LEFT JOIN settlement_receivable r", Rows: [][]driver.Value{
			{"plan-1", int64(1), "2099-01-01", "100.00", "PENDING_CONFIRMATION", "", "0", "0", "0", "0", int64(0)},
			{"plan-2", int64(2), "2099-02-01", "50.00", "PENDING_CONFIRMATION", "", "0", "0", "0", "0", int64(0)},
		}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_snapshot"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_snapshot SET financial_status='SUPERSEDED'"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable_plan SET contract_snapshot_id=?,updated_at"},
		scriptdb.Step{Kind: "exec", SQL: "DELETE FROM settlement_receivable WHERE"},
		scriptdb.Step{Kind: "exec", SQL: "DELETE FROM settlement_receivable_plan WHERE"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: &removeAudit},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_stream"},
		scriptdb.Step{Kind: "commit"},
	)
	svc := &Service{DB: db}
	result, err := svc.IngestContract(context.Background(), "contract_management", "tenant-a", revisionContractEvent("EVT-10", 2, "100.00", revisionTerm(1, "2099-01-01", "100.00")))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.RevisionHeld {
		t.Fatalf("删除分期结果=%+v", result)
	}
	assertRevisionAudit(t, removeAudit, "SETTLEMENT_PLAN_ADJUSTED", `"action":"REMOVED"`, `"installment_no":2`, `"old_amount":"50.00"`)
}
