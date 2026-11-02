package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// planCoverageTestEnv 固定两个阈值环境变量，避免宿主环境影响边界断言。
// 参数：t为测试上下文。
// 返回值：无。
func planCoverageTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv(planCoverageGapEnvName, "")
	t.Setenv(planCoverageDropEnvName, "")
}

// TestPlanCoverageThresholdBoundary 验证差额与单期降幅阈值的"等于/超过"边界。
// 参数：t为测试上下文。
// 返回值：无；任一边界判定不符时失败。
func TestPlanCoverageThresholdBoundary(t *testing.T) {
	planCoverageTestEnv(t)
	for _, scenario := range []struct {
		name                     string
		contract, existing, plan string
		previous                 string
		wantGap, wantPercent     string
		wantExceeds, wantGapTrip bool
		wantDropTrip             bool
	}{
		{name: "差额恰好等于阈值", contract: "100.00", existing: "95.00", plan: "0", wantGap: "5.00", wantPercent: "5.00"},
		{name: "差额刚超过阈值", contract: "100.00", existing: "94.99", plan: "0", wantGap: "5.01", wantPercent: "5.01", wantExceeds: true, wantGapTrip: true},
		{name: "差额为零", contract: "100.00", existing: "95.00", plan: "5.00", wantGap: "0.00", wantPercent: "0.00"},
		{name: "计划超计为负差额", contract: "100.00", existing: "100.00", plan: "1.00", wantGap: "-1.00", wantPercent: "1.00"},
		{name: "单期降幅恰好50%", contract: "100.00", existing: "50.00", plan: "50.00", previous: "100.00", wantGap: "0.00", wantPercent: "0.00"},
		{name: "单期降幅刚超过50%", contract: "100.00", existing: "50.01", plan: "49.99", previous: "100.00", wantGap: "0.00", wantPercent: "0.00", wantExceeds: true, wantDropTrip: true},
		{name: "单期上调不触发降幅", contract: "100.00", existing: "0", plan: "100.00", previous: "100.00", wantGap: "0.00", wantPercent: "0.00"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			coverage, err := evaluatePlanCoverage(scenario.contract, scenario.existing, scenario.plan, scenario.previous, 1)
			if err != nil {
				t.Fatal(err)
			}
			if coverage.Gap != scenario.wantGap || coverage.GapPercent != scenario.wantPercent {
				t.Fatalf("gap=%s pct=%s want=%s/%s", coverage.Gap, coverage.GapPercent, scenario.wantGap, scenario.wantPercent)
			}
			if coverage.Exceeds != scenario.wantExceeds {
				t.Fatalf("exceeds=%v want=%v triggered=%v", coverage.Exceeds, scenario.wantExceeds, coverage.Triggered)
			}
			if got := containsTrigger(coverage.Triggered, planCoverageTriggerGap); got != scenario.wantGapTrip {
				t.Fatalf("gap trigger=%v want=%v (%v)", got, scenario.wantGapTrip, coverage.Triggered)
			}
			if got := containsTrigger(coverage.Triggered, planCoverageTriggerDrop); got != scenario.wantDropTrip {
				t.Fatalf("drop trigger=%v want=%v (%v)", got, scenario.wantDropTrip, coverage.Triggered)
			}
		})
	}
}

// TestPlanCoverageThresholdsFromEnvironment 验证阈值可由环境变量收紧，非法值回到默认。
// 参数：t为测试上下文。
// 返回值：无；阈值读取或回落行为不符时失败。
func TestPlanCoverageThresholdsFromEnvironment(t *testing.T) {
	planCoverageTestEnv(t)
	t.Setenv(planCoverageGapEnvName, "2")
	t.Setenv(planCoverageDropEnvName, "10")
	coverage, err := evaluatePlanCoverage("100.00", "98.00", "0", "", 0)
	if err != nil || coverage.Exceeds {
		t.Fatalf("自定义2%%阈值应放行等于阈值: %+v err=%v", coverage, err)
	}
	coverage, err = evaluatePlanCoverage("100.00", "97.99", "0", "", 0)
	if err != nil || !coverage.Exceeds {
		t.Fatalf("自定义2%%阈值应拒绝超过阈值: %+v err=%v", coverage, err)
	}
	coverage, err = evaluatePlanCoverage("100.00", "95.00", "0", "100.00", 0)
	if err != nil || !coverage.Exceeds {
		t.Fatalf("自定义10%%降幅应判定超阈值: %+v err=%v", coverage, err)
	}
	t.Setenv(planCoverageGapEnvName, "非法")
	t.Setenv(planCoverageDropEnvName, "120")
	coverage, err = evaluatePlanCoverage("100.00", "95.50", "0", "", 0)
	if err != nil || coverage.Exceeds {
		t.Fatalf("非法阈值必须回到默认5%%: %+v err=%v", coverage, err)
	}
}

// containsTrigger 判断覆盖事实是否命中指定触发原因。
// 参数：triggered为触发原因列表，want为原因。
// 返回值：命中返回真。
func containsTrigger(triggered []string, want string) bool {
	for _, item := range triggered {
		if item == want {
			return true
		}
	}
	return false
}

// TestPlanCoverageGuardRequiresFinanceOverride 验证超阈值时缺少 override 权限被拒绝，
// 持有权限时放行，且合同金额上限不因 override 放松。
// 参数：t为测试上下文。
// 返回值：无；守卫判定不符时失败。
func TestPlanCoverageGuardRequiresFinanceOverride(t *testing.T) {
	planCoverageTestEnv(t)
	limited := Principal{TenantID: "tenant-a", UserID: "sales-a", Permissions: map[string]bool{"settlement.receivable.create": true, "settlement.receivable.adjust": true}}
	override := Principal{TenantID: "tenant-a", UserID: "finance-manager", Permissions: map[string]bool{"settlement.receivable.create": true, "settlement.receivable.adjust": true, "settlement.finance.override": true}}
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_amount,source_contract_no", Rows: [][]driver.Value{{"100.00", "C-1"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan", Rows: [][]driver.Value{{"90.00", int64(1)}}},
		scriptdb.Step{Kind: "rollback"},
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_amount,source_contract_no", Rows: [][]driver.Value{{"100.00", "C-1"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan", Rows: [][]driver.Value{{"90.00", int64(1)}}},
		scriptdb.Step{Kind: "rollback"},
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_amount,source_contract_no", Rows: [][]driver.Value{{"100.00", "C-1"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan", Rows: [][]driver.Value{{"90.00", int64(1)}}},
		scriptdb.Step{Kind: "rollback"},
	)
	svc := &Service{DB: db}
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := svc.requirePlanCoverage(ctx, tx, limited, "snapshot-1", "", "1.00", "")
	if !errors.Is(err, ErrConflict) || !coverage.Exceeds {
		t.Fatalf("缺少override必须拒绝: err=%v coverage=%+v", err, coverage)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err = svc.requirePlanCoverage(ctx, tx, override, "snapshot-1", "", "1.00", "")
	if err != nil || !coverage.Exceeds || coverage.Gap != "9.00" {
		t.Fatalf("override必须放行并返回差额: err=%v coverage=%+v", err, coverage)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// 合计超过合同金额时，override 也不得放行。
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.requirePlanCoverage(ctx, tx, override, "snapshot-1", "", "11.00", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("超合同金额必须拒绝: %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// TestCreateManualPlanWritesCoverageDeviationAudit 验证人工新增计划造成的差额
// 在 override 放行后仍写入显式审计。
// 参数：t为测试上下文。
// 返回值：无；缺少差额审计或审计载荷不完整时失败。
func TestCreateManualPlanWritesCoverageDeviationAudit(t *testing.T) {
	planCoverageTestEnv(t)
	var auditArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_amount,source_contract_no", Rows: [][]driver.Value{{"100.00", "C-1"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan", Rows: [][]driver.Value{{"90.00", int64(0)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_receivable_plan"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: &auditArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "commit"},
	)
	svc := &Service{DB: db}
	p := Principal{TenantID: "tenant-a", UserID: "finance-manager", Permissions: map[string]bool{"settlement.receivable.create": true, "settlement.finance.override": true}}
	if _, err := svc.CreateManualPlan(context.Background(), p, "key-1", ManualPlanInput{ContractSnapshotID: "snapshot-1", InstallmentNo: 2, DueDate: "2099-12-01", PlannedAmount: "1.00", Reason: "历史分期补录"}); err != nil {
		t.Fatal(err)
	}
	assertCoverageDeviationAudit(t, auditArgs, "CREATE_MANUAL_PLAN")
}

// TestCreateManualPlanWithoutOverrideRejectsExtremeDrop 验证手工计划造成超阈值差额时，
// 缺少 override 权限在写库前失败关闭。
// 参数：t为测试上下文。
// 返回值：无；未拒绝或发生写入时失败。
func TestCreateManualPlanWithoutOverrideRejectsExtremeDrop(t *testing.T) {
	planCoverageTestEnv(t)
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_amount,source_contract_no", Rows: [][]driver.Value{{"100.00", "C-1"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan", Rows: [][]driver.Value{{"90.00", int64(1)}}},
		scriptdb.Step{Kind: "rollback"},
	)
	svc := &Service{DB: db}
	p := Principal{TenantID: "tenant-a", UserID: "sales-a", Permissions: map[string]bool{"settlement.receivable.create": true}}
	_, err := svc.CreateManualPlan(context.Background(), p, "key-1", ManualPlanInput{ContractSnapshotID: "snapshot-1", InstallmentNo: 2, DueDate: "2099-12-01", PlannedAmount: "1.00", Reason: "把某期下调到1元"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("缺少override必须拒绝: %v", err)
	}
}

// TestConfirmPlanChecksCoverageWithoutAmountChange 验证确认计划不再只在金额变化时
// 校验覆盖：金额不变但差额超阈值同样要求 override。
// 参数：t为测试上下文。
// 返回值：无；未无条件校验时失败。
func TestConfirmPlanChecksCoverageWithoutAmountChange(t *testing.T) {
	planCoverageTestEnv(t)
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan rp JOIN settlement_contract_snapshot cs", Rows: [][]driver.Value{{"snapshot-1", "2099-12-01", "60.00", "CNY"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_amount,source_contract_no", Rows: [][]driver.Value{{"100.00", "C-1"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan", Rows: [][]driver.Value{{"0.00", int64(1)}}},
		scriptdb.Step{Kind: "rollback"},
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan rp JOIN settlement_contract_snapshot cs", Rows: [][]driver.Value{{"snapshot-1", "2099-12-01", "60.00", "CNY"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_amount,source_contract_no", Rows: [][]driver.Value{{"100.00", "C-1"}}},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan", Rows: [][]driver.Value{{"0.00", int64(1)}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable_plan SET due_date=?"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_receivable("},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "commit"},
	)
	svc := &Service{DB: db}
	limited := Principal{TenantID: "tenant-a", UserID: "sales-a", Permissions: map[string]bool{"settlement.receivable.confirm": true}}
	if err := svc.ConfirmPlan(context.Background(), limited, "plan-1", PlanConfirmationInput{Version: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("金额不变但差额超阈值必须拒绝: %v", err)
	}
	finance := Principal{TenantID: "tenant-a", UserID: "finance-manager", Permissions: map[string]bool{"settlement.receivable.confirm": true, "settlement.finance.override": true}}
	if err := svc.ConfirmPlan(context.Background(), finance, "plan-1", PlanConfirmationInput{Version: 1}); err != nil {
		t.Fatalf("override应放行确认: %v", err)
	}
}

// assertCoverageDeviationAudit 断言差额审计载荷完整且风险等级符合阈值命中。
// 参数：t为测试上下文，args为捕获的审计参数，wantOperation为入口标识。
// 返回值：无；载荷不符时失败。
func assertCoverageDeviationAudit(t *testing.T, args []driver.NamedValue, wantOperation string) {
	t.Helper()
	if len(args) < 8 {
		t.Fatalf("差额审计参数不足: %d", len(args))
	}
	eventType, ok := args[4].Value.(string)
	if !ok || eventType != "SETTLEMENT_PLAN_COVERAGE_DEVIATION" {
		t.Fatalf("审计事件类型=%v", args[4].Value)
	}
	payload, ok := args[7].Value.([]byte)
	if !ok {
		t.Fatalf("审计载荷类型=%T", args[7].Value)
	}
	body := string(payload)
	for _, want := range []string{`"operation":"` + wantOperation + `"`, `"contract_amount":"100.00"`, `"plan_total":"91.00"`, `"gap":"9.00"`, `"gap_percent":"9.00"`, `"risk_level":"HIGH"`, `"reason":"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("差额审计载荷缺少 %s: %s", want, body)
		}
	}
}

// TestPlanCoverageReadViewUsesTenantAndScope 验证只读差额查询按租户与责任范围绑定。
// 参数：t为测试上下文。
// 返回值：无；查询参数或差额计算不符时失败。
func TestPlanCoverageReadViewUsesTenantAndScope(t *testing.T) {
	planCoverageTestEnv(t)
	var args []driver.NamedValue
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_snapshot cs LEFT JOIN settlement_receivable_plan rp", Args: &args, Rows: [][]driver.Value{
		{"snapshot-1", "C-1", "100.00", "94.50", int64(1)},
	}})
	svc := &Service{DB: db}
	p := Principal{TenantID: "tenant-a", UserID: "collector", Permissions: map[string]bool{"settlement.receivable.read": true}}
	items, err := svc.ListPlanCoverageDeviations(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Gap != "5.50" || items[0].GapPercent != "5.50" || !items[0].ExceedsThreshold || !items[0].ContractEventPlan {
		t.Fatalf("覆盖视图=%+v", items)
	}
	if len(args) == 0 || args[0].Value != "tenant-a" {
		t.Fatalf("租户绑定参数=%+v", args)
	}
}
