package service

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
	"testing"
)

// TestFinanceApprovalStopsBeforeAttempt 验证普通财务审核遇加签条件只转换状态。
// 参数：t 测试上下文。
// 返回值：无。
func TestFinanceApprovalStopsBeforeAttempt(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT submitted_by", Rows: [][]driver.Value{{"applicant"}}}, scriptdb.Step{Kind: "query", SQL: "SELECT required_review", Rows: [][]driver.Value{{true}}}, scriptdb.Step{Kind: "exec", SQL: "status='SPECIAL_REVIEW'"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}, scriptdb.Step{Kind: "commit"})
	attempt, err := (&Service{DB: db}).ApproveInvoiceRequest(t.Context(), Principal{TenantID: "tenant", UserID: "reviewer"}, "request", 1)
	if err != nil || attempt != "" {
		t.Fatalf("attempt=%s err=%v", attempt, err)
	}
}

// TestSpecialReviewRejectsApplicant 验证申请人不能通过总监角色自审。
// 参数：t 测试上下文。
// 返回值：无。
func TestSpecialReviewRejectsApplicant(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "status='SPECIAL_REVIEW'", Rows: [][]driver.Value{{"applicant", "snapshot"}}}, scriptdb.Step{Kind: "rollback"})
	_, err := (&Service{DB: db}).ReviewSpecialInvoice(t.Context(), Principal{TenantID: "tenant", UserID: "applicant", Permissions: map[string]bool{"settlement.invoice.special_approve": true}}, "request", 2, true, "")
	if !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}

// TestVoucherUnconfiguredIsNotSuccess 验证未配置ERP不会写入成功回执。
// 参数：t 测试上下文。
// 返回值：无。
func TestVoucherUnconfiguredIsNotSuccess(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT payload,status", Rows: [][]driver.Value{{[]byte(`{"amount":"10"}`), "NOT_CONFIGURED"}}}, scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_accounting_voucher"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}, scriptdb.Step{Kind: "commit"})
	state, err := (&Service{DB: db}).RetryAccountingVoucher(t.Context(), Principal{TenantID: "tenant", Permissions: map[string]bool{"settlement.voucher.manage": true}}, "voucher")
	if err != nil || state != "NOT_CONFIGURED" {
		t.Fatalf("%s %v", state, err)
	}
}

type rejectedERP struct{}

// SendVoucher 模拟真实适配器返回失败，验证本地状态保留失败。
// 参数：ctx 上下文，id 幂等凭证标识，payload 本地凭证。
// 返回值：空外部编号及适配失败。
func (rejectedERP) SendVoucher(ctx context.Context, id string, payload json.RawMessage) (string, error) {
	return "", errors.New("provider unavailable")
}

// TestVoucherProviderFailureIsRetryable 验证适配失败保存FAILED而非伪成功。
// 参数：t 测试上下文。
// 返回值：无。
func TestVoucherProviderFailureIsRetryable(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT payload,status", Rows: [][]driver.Value{{[]byte(`{}`), "FAILED"}}}, scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_accounting_voucher"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}, scriptdb.Step{Kind: "commit"})
	state, err := (&Service{DB: db, ERPProvider: rejectedERP{}}).RetryAccountingVoucher(t.Context(), Principal{TenantID: "tenant", Permissions: map[string]bool{"settlement.voucher.manage": true}}, "voucher")
	if err != nil || state != "FAILED" {
		t.Fatalf("%s %v", state, err)
	}
}

// TestDifferenceApproverCannotBeSubmitter 验证差异处置需要非申请人审批。
// 参数：t 测试上下文。
// 返回值：无。
func TestDifferenceApproverCannotBeSubmitter(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT requested_by", Rows: [][]driver.Value{{"actor", "resolver", "receipt", "UNKNOWN", "REFUND_PENDING", "10.00"}}}, scriptdb.Step{Kind: "rollback"})
	err := (&Service{DB: db}).ApproveReceiptDifference(t.Context(), Principal{TenantID: "tenant", UserID: "actor", Permissions: map[string]bool{"settlement.difference.approve": true}}, "difference", 2)
	if !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}
