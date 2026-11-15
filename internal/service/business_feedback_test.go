package service

import (
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestPlanAdjustmentVersionFeedback 用途：验证计划版本或状态变化不误报金额不足。
// 参数：t 为只运行脚本SQL驱动的测试上下文。
// 返回值：无；原冲突分类或刷新指引丢失时失败。
func TestPlanAdjustmentVersionFeedback(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT rp.contract_snapshot_id", Columns: 6},
		scriptdb.Step{Kind: "rollback"})
	err := (&Service{DB: db}).AdjustPlan(t.Context(), Principal{
		TenantID: "tenant", Permissions: map[string]bool{"settlement.receivable.adjust": true},
	}, "plan", PlanConfirmationInput{Version: 1, DueDate: "2026-12-01", PlannedAmount: "50", Reason: "核对计划"})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "版本") ||
		!strings.Contains(err.Error(), "刷新") || strings.Contains(err.Error(), "余额不足") {
		t.Fatalf("版本拒绝未明确刷新核对: %v", err)
	}
}

// TestPlanAdjustmentFeedbackBounds 用途：验证已占用金额下限与版本冲突不会混淆。
// 参数：t 为只运行脚本SQL驱动的测试上下文。
// 返回值：无；金额拒绝原因不具体或错误分类改变时失败。
func TestPlanAdjustmentFeedbackBounds(t *testing.T) {
	for _, tc := range []struct{ used, invoiced, want string }{
		{"60.00", "", "已核销及核减金额 60.00"},
		{"0.00", "70.00", "已开票及预留金额 70.00"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			steps := []scriptdb.Step{{Kind: "begin"},
				{Kind: "query", SQL: "SELECT rp.contract_snapshot_id", Rows: [][]driver.Value{{"snapshot", "2026-11-01", "100.00", "receivable", tc.used, "100.00"}}},
				{Kind: "query", SQL: "SELECT migration_state", Rows: [][]driver.Value{{"RELEASED"}}},
				{Kind: "query", SQL: "SELECT migration_state", Rows: [][]driver.Value{{"RELEASED"}}},
			}
			if tc.invoiced != "" {
				steps = append(steps, scriptdb.Step{Kind: "query", SQL: "SELECT invoiced_amount+", Rows: [][]driver.Value{{tc.invoiced}}})
			}
			steps = append(steps, scriptdb.Step{Kind: "rollback"})
			svc := &Service{DB: scriptdb.Open(t, steps...)}
			p := Principal{TenantID: "tenant", UserID: "finance", Permissions: map[string]bool{"settlement.receivable.adjust": true}}
			err := svc.AdjustPlan(t.Context(), p, "plan", PlanConfirmationInput{
				Version: 1, DueDate: "2026-12-01", PlannedAmount: "50.00", Reason: "核对计划金额",
			})
			if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("未明确金额下限: %v", err)
			}
		})
	}
}

// TestInvoiceFeedbackNamesLineAndRelation 用途：验证发票关系校验说明真实行号和金额关系。
// 参数：t 为不连接数据库的测试上下文。
// 返回值：无；关系或小数格式的公开原因缺失时失败。
func TestInvoiceFeedbackNamesLineAndRelation(t *testing.T) {
	for _, tc := range []struct{ field, value, want string }{
		{"tax", "0.61", "第 2 行税额应为 1.30"},
		{"total", "11.31", "第 2 行价税合计应为 11.30"},
		{"quantity", "1.1234567", "第 2 行数量"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			line := invoiceRequestFixture(nil, "ELECTRONIC_NORMAL").Items[0]
			items := []InvoiceItemInput{line, line}
			switch tc.field {
			case "tax":
				items[1].TaxAmount = tc.value
			case "total":
				items[1].AmountInclTax = tc.value
			case "quantity":
				items[1].Quantity = tc.value
			}
			_, _, _, _, err := normalizeInvoiceItems(items)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("明细提示没有真实行和关系: %v", err)
			}
		})
	}
}

// TestApprovalFeedbackPreservesSeparation 用途：验证已有异人审核门禁返回明确安全原因。
// 参数：t 为脚本SQL测试上下文，不访问真实账务数据。
// 返回值：无；门禁被放松或原因缺失时失败。
func TestApprovalFeedbackPreservesSeparation(t *testing.T) {
	for _, tc := range []struct {
		applicant, ordinary, user, want string
		kind                            error
	}{
		{"applicant", "finance", "applicant", "申请人不能加签", ErrForbidden},
		{"applicant", "", "director", "缺少普通审核人", ErrConflict},
		{"applicant", "director", "director", "普通审核人与总监加签必须不同人", ErrForbidden},
	} {
		t.Run(tc.want, func(t *testing.T) {
			db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"},
				scriptdb.Step{Kind: "query", SQL: "status='SPECIAL_REVIEW'", Rows: [][]driver.Value{{tc.applicant, tc.ordinary, "snapshot"}}},
				scriptdb.Step{Kind: "query", SQL: "SELECT migration_state",
					Rows: [][]driver.Value{{"RELEASED"}}},
				scriptdb.Step{Kind: "query", SQL: "SELECT migration_state",
					Rows: [][]driver.Value{{"RELEASED"}}},
				scriptdb.Step{Kind: "rollback"})
			_, err := (&Service{DB: db}).ReviewSpecialInvoice(t.Context(), Principal{TenantID: "tenant", UserID: tc.user,
				Permissions: map[string]bool{"settlement.invoice.special_approve": true}}, "request", 1, true, "")
			if !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("审批独立性或原因不符: %v", err)
			}
		})
	}
}

// TestCoverageFeedbackReasons 用途：验证合同上限与覆盖阈值返回不同安全原因。
// 参数：t 为脚本SQL驱动测试上下文，阈值使用既有默认配置。
// 返回值：无；原因混淆或公开说明缺少实际金额时失败。
func TestCoverageFeedbackReasons(t *testing.T) {
	planCoverageTestEnv(t)
	for _, tc := range []struct{ planned, reason, want string }{
		{"11.00", "PLAN_CONTRACT_LIMIT", "计划合计 101.00 不能超过合同金额 100.00"},
		{"1.00", "PLAN_COVERAGE_THRESHOLD", "差额 9.00 / 9.00%"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"},
				scriptdb.Step{Kind: "query", SQL: "SELECT contract_amount,source_contract_no", Rows: [][]driver.Value{{"100.00", "C-1"}}},
				scriptdb.Step{Kind: "query", SQL: "FROM settlement_receivable_plan", Rows: [][]driver.Value{{"90.00", int64(1)}}},
				scriptdb.Step{Kind: "rollback"})
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			_, err = (&Service{DB: db}).requirePlanCoverage(t.Context(), tx,
				Principal{TenantID: "tenant"}, "snapshot", "", tc.planned, "")
			message, details, public := BusinessFeedback(err)
			if !errors.Is(err, ErrConflict) || !public || details["reason_code"] != tc.reason ||
				!strings.Contains(message, tc.want) {
				t.Fatalf("金额拒绝未安全区分: %v %+v", err, details)
			}
		})
	}
}
