package dunning

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestActiveCaseConflictBindsPlanAndReceivable 验证重叠检查按租户、应收/计划和
// 策略版本绑定，并排除当前策略版本自身的案件。
// 参数：t 测试上下文。
// 返回值：无；查询未绑定对象或未排除自身时失败。
func TestActiveCaseConflictBindsPlanAndReceivable(t *testing.T) {
	var args []driver.NamedValue
	var query string
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_dunning_case", Args: &args, SQLOut: &query, Rows: [][]driver.Value{{"existing-case"}}})
	id, err := activeCaseConflict(context.Background(), db, "tenant-a", "", "plan-1", "policy-1", 2)
	if err != nil || id != "existing-case" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if len(args) != 5 || args[0].Value != "tenant-a" || args[1].Value != "plan-1" || args[2].Value != "policy-1" || args[3].Value != int64(2) || args[4].Value != "plan-1" {
		t.Fatalf("plan overlap query args=%+v", args)
	}
	if !strings.Contains(query, "status='ACTIVE'") || !strings.Contains(query, "NOT (dunning_policy_id=? AND policy_version=? AND COALESCE(plan_id,'')=?)") {
		t.Fatalf("overlap query must only match foreign ACTIVE cases: %s", query)
	}
}

// TestActiveCaseConflictDerivesPlanFromReceivable 验证扫描器只持有应收标识时，
// 通过应收关联计划识别同一催收义务上的案件。
// 参数：t 测试上下文。
// 返回值：无；关联计划未被纳入冲突判断时失败。
func TestActiveCaseConflictDerivesPlanFromReceivable(t *testing.T) {
	var args []driver.NamedValue
	var query string
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_dunning_case", Args: &args, SQLOut: &query, Rows: [][]driver.Value{{"existing-case"}}})
	if _, err := activeCaseConflict(context.Background(), db, "tenant-a", "recv-1", "", "policy-1", 1); err != nil {
		t.Fatal(err)
	}
	if len(args) != 7 || args[0].Value != "tenant-a" || args[1].Value != "recv-1" || args[2].Value != "tenant-a" || args[3].Value != "recv-1" || args[4].Value != "policy-1" || args[5].Value != int64(1) || args[6].Value != "" {
		t.Fatalf("receivable overlap query args=%+v", args)
	}
	if !strings.Contains(query, "(SELECT rp.receivable_plan_id FROM settlement_receivable rp") {
		t.Fatalf("overlap query must derive the linked plan: %s", query)
	}
}

// TestActiveCaseConflictSkipsEmptyObligation 验证没有应收或计划时不做无意义查询。
// 参数：t 测试上下文。
// 返回值：无；空对象仍访问数据库时失败。
func TestActiveCaseConflictSkipsEmptyObligation(t *testing.T) {
	db := scriptdb.Open(t)
	if id, err := activeCaseConflict(context.Background(), db, "tenant-a", " ", "", "policy-1", 1); err != nil || id != "" {
		t.Fatalf("empty obligation must not query: id=%q err=%v", id, err)
	}
}

// TestActiveCaseConflictPropagatesDatabaseFailure 验证检查失败时失败关闭，
// 调用方不能把未知状态当成"无冲突"继续创建。
// 参数：t 测试上下文。
// 返回值：无；数据库错误被吞掉时失败。
func TestActiveCaseConflictPropagatesDatabaseFailure(t *testing.T) {
	denied := errors.New("database unavailable")
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_dunning_case", Err: denied})
	if _, err := activeCaseConflict(context.Background(), db, "tenant-a", "recv-1", "", "policy-1", 1); !errors.Is(err, denied) {
		t.Fatalf("err=%v want database failure", err)
	}
}

// TestActiveCaseConflictReturnsEmptyWithoutRows 验证无 ACTIVE 案件时允许创建。
// 参数：t 测试上下文。
// 返回值：无；无行被误判为冲突时失败。
func TestActiveCaseConflictReturnsEmptyWithoutRows(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_dunning_case", Err: sql.ErrNoRows})
	if id, err := activeCaseConflict(context.Background(), db, "tenant-a", "", "plan-1", "policy-1", 1); err != nil || id != "" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

// TestScannerSkipsInitialCaseWhenObligationAlreadyActive 验证扫描器立案前
// 复用既有 ACTIVE 案件：不再插入第二个案件，也不创建动作。
// 参数：t 测试上下文。
// 返回值：无；冲突时仍写案件或动作时失败。
func TestScannerSkipsInitialCaseWhenObligationAlreadyActive(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_dunning_case", Rows: [][]driver.Value{{"existing-case"}}},
		scriptdb.Step{Kind: "commit"},
	)
	scanner := &Scanner{DB: db}
	if err := scanner.createInitial(context.Background(), candidate{TenantID: "tenant-a", ReceivableID: "recv-1", PlanID: "plan-1", PolicyID: "policy-1", PolicyVersion: 1, ActionType: "REMIND", RepeatDays: 7}, "collector"); err != nil {
		t.Fatal(err)
	}
}

// TestScannerCreatesInitialCaseWhenNoOverlap 验证无冲突时仍按原流程立案并派发动作。
// 参数：t 测试上下文。
// 返回值：无；重叠校验误伤正常立案时失败。
func TestScannerCreatesInitialCaseWhenNoOverlap(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_dunning_case", Rows: [][]driver.Value{}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT IGNORE INTO settlement_dunning_case"},
		scriptdb.Step{Kind: "query", SQL: "SELECT cs.migration_state", Rows: [][]driver.Value{{"RELEASED", int64(0)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_dunning_action"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_local_notification"},
		scriptdb.Step{Kind: "commit"},
	)
	scanner := &Scanner{DB: db}
	if err := scanner.createInitial(context.Background(), candidate{TenantID: "tenant-a", ReceivableID: "recv-1", PolicyID: "policy-1", PolicyVersion: 1, ActionType: "REMIND", RepeatDays: 7}, "collector"); err != nil {
		t.Fatal(err)
	}
}
