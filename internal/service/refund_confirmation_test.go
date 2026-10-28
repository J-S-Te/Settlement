package service

import (
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// refundTestPrincipal 构造具有财务全范围操作权限的测试主体。
// 参数：无。
// 返回值：固定测试租户会计主体。
func refundTestPrincipal() Principal {
	return Principal{TenantID: "refund-tenant", UserID: "accountant", Permissions: map[string]bool{"settlement.difference.manage": true}}
}

// refundTestInput 构造确定的历史退款完成事实。
// 参数：无。
// 返回值：有效版本及带时区的退款凭证。
func refundTestInput() ConfirmRefundRequest {
	return ConfirmRefundRequest{Version: 3, Reference: "银行退款-1", CompletedAt: "2020-01-02T10:00:00+08:00", Reason: "线下退款已完成"}
}

// TestRefundRejectsInvalidFactsBeforeDatabase 验证非法凭证和未来时间在事务前被拒绝。
// 参数：t 测试上下文。
// 返回值：无；遗漏任何输入边界时失败。
func TestRefundRejectsInvalidFactsBeforeDatabase(t *testing.T) {
	for _, mutate := range []func(*ConfirmRefundRequest){
		func(in *ConfirmRefundRequest) { in.Version = 0 },
		func(in *ConfirmRefundRequest) { in.Reference = " " },
		func(in *ConfirmRefundRequest) { in.Reference = strings.Repeat("凭", 43) },
		func(in *ConfirmRefundRequest) { in.Reason = " " },
		func(in *ConfirmRefundRequest) { in.Reason = strings.Repeat("a", 501) },
		func(in *ConfirmRefundRequest) { in.CompletedAt = "2020-02-30T00:00:00Z" },
		func(in *ConfirmRefundRequest) { in.CompletedAt = "2020-01-02" },
		func(in *ConfirmRefundRequest) { in.CompletedAt = time.Now().Add(time.Hour).Format(time.RFC3339) },
	} {
		in := refundTestInput()
		mutate(&in)
		if err := (&Service{}).ConfirmReceiptRefund(t.Context(), refundTestPrincipal(), "difference", in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid input accepted: %+v err=%v", in, err)
		}
	}
	if err := (&Service{}).ConfirmReceiptRefund(t.Context(), Principal{}, "difference", refundTestInput()); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	limited := roleFlowPrincipal("settlement_sales", "sales", "tenant")
	limited.Permissions = map[string]bool{"settlement.difference.manage": true}
	if err := (&Service{}).ConfirmReceiptRefund(t.Context(), limited, "difference", refundTestInput()); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}

// TestRefundRequiresIndependentApprovedCurrentState 验证重复、旧版和未独立审批均只回滚。
// 参数：t 测试上下文。
// 返回值：无；非法状态产生写入时SQL脚本会失败。
func TestRefundRequiresIndependentApprovedCurrentState(t *testing.T) {
	cases := []struct {
		name, status, resolution, approver string
		version                            int
		missing                            bool
		want                               error
	}{
		{"repeated", "REFUND_CONFIRMED", "REFUND_PENDING", "manager", 4, false, ErrConflict},
		{"stale", "REFUND_PENDING", "REFUND_PENDING", "manager", 4, false, ErrConflict},
		{"held", "HELD", "REFUND_PENDING", "manager", 3, false, ErrConflict},
		{"unapproved", "REFUND_PENDING", "REFUND_PENDING", "", 3, false, ErrConflict},
		{"self-approved", "REFUND_PENDING", "REFUND_PENDING", "accountant", 3, false, ErrConflict},
		{"wrong-resolution", "REFUND_PENDING", "CARRY_FORWARD", "manager", 3, false, ErrConflict},
		{"hidden", "", "", "", 0, true, ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := [][]driver.Value{{c.status, c.resolution, "accountant", "accountant", c.approver, "receipt", int64(c.version)}}
			if c.missing {
				rows = nil
			}
			db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "FOR UPDATE", Rows: rows, Columns: 7}, scriptdb.Step{Kind: "rollback"})
			err := (&Service{DB: db}).ConfirmReceiptRefund(t.Context(), refundTestPrincipal(), "difference", refundTestInput())
			if !errors.Is(err, c.want) {
				t.Fatalf("want=%v got=%v", c.want, err)
			}
		})
	}
}

// TestRefundTransactionPreservesMoneyAndRollsBackAuditFailure 验证确认不写回款且审计失败会回滚。
// 参数：t 测试上下文。
// 返回值：无；提前提交、余额变动或吞审计错误时失败。
func TestRefundTransactionPreservesMoneyAndRollsBackAuditFailure(t *testing.T) {
	for _, failAudit := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "audit-failure"}[failAudit], func(t *testing.T) {
			steps := []scriptdb.Step{{Kind: "begin"}, {Kind: "query", SQL: "FOR UPDATE", Rows: [][]driver.Value{{"REFUND_PENDING", "REFUND_PENDING", "accountant", "accountant", "manager", "receipt", int64(3)}}}, {Kind: "query", SQL: "DATE_FORMAT(receipt_date", Rows: [][]driver.Value{{"2020-01-01"}}}, {Kind: "exec", SQL: "status='REFUND_CONFIRMED'"}, {Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, {Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}}
			auditFailure := errors.New("audit unavailable")
			if failAudit {
				steps[len(steps)-1].Err = auditFailure
				steps = append(steps, scriptdb.Step{Kind: "rollback"})
			} else {
				steps = append(steps, scriptdb.Step{Kind: "commit"})
			}
			db := scriptdb.Open(t, steps...)
			err := (&Service{DB: db}).ConfirmReceiptRefund(t.Context(), refundTestPrincipal(), "difference", refundTestInput())
			if failAudit && !errors.Is(err, auditFailure) || !failAudit && err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRefundCannotPrecedeReceiptDate 验证时间以UTC比较且不能早于回款日期。
// 参数：t 测试上下文。
// 返回值：无；日期错误时不得保存事实或审计。
func TestRefundCannotPrecedeReceiptDate(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "FOR UPDATE", Rows: [][]driver.Value{{"REFUND_PENDING", "REFUND_PENDING", "accountant", "accountant", "manager", "receipt", int64(3)}}}, scriptdb.Step{Kind: "query", SQL: "DATE_FORMAT(receipt_date", Rows: [][]driver.Value{{"2020-01-03"}}}, scriptdb.Step{Kind: "rollback"})
	if err := (&Service{DB: db}).ConfirmReceiptRefund(t.Context(), refundTestPrincipal(), "difference", refundTestInput()); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	in, completed, err := normalizeRefundConfirmation(refundTestInput(), time.Now())
	if err != nil || in.Reference != "银行退款-1" || completed.Format(time.RFC3339) != "2020-01-02T02:00:00Z" {
		t.Fatalf("in=%+v completed=%s err=%v", in, completed, err)
	}
}

// TestFinanceQueryRejectsUnboundedServiceCalls 验证直接服务调用也执行分页和筛选边界。
// 参数：t 测试上下文。
// 返回值：无；非法查询触及数据库或被接受时失败。
func TestFinanceQueryRejectsUnboundedServiceCalls(t *testing.T) {
	for _, vouchers := range []bool{false, true} {
		p := refundTestPrincipal()
		p.Permissions["settlement.voucher.manage"] = true
		for _, in := range []FinanceRecordQuery{{Limit: 0}, {Limit: 101}, {Limit: 1, Offset: -1}, {Limit: 1, Offset: 100001}, {Limit: 1, Keyword: strings.Repeat("a", 129)}, {Limit: 1, Status: "UNKNOWN"}} {
			if _, err := (&Service{}).ListFinanceRecords(t.Context(), p, vouchers, in); !errors.Is(err, ErrInvalid) {
				t.Fatalf("query=%+v err=%v", in, err)
			}
		}
	}
	query, err := financeRecordQuery(false, nil)
	if err != nil || query.Limit != 100 || query.Offset != 0 {
		t.Fatalf("legacy defaults=%+v err=%v", query, err)
	}
}
