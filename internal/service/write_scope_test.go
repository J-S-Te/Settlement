package service

import (
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// scopedManager 返回持有指定能力但只有组织级数据范围的主体（范围内责任未绑定）。
// 参数：permission 为写操作能力码。
// 返回值：已版本化的受限财务经理主体。
func scopedManager(permission string) Principal {
	return Principal{
		TenantID: "tenant", UserID: "finance-manager", CatalogVersion: "catalog-test",
		Permissions: map[string]bool{permission: true},
		Roles:       []string{"settlement_finance_manager"},
		DataScopes:  []DataScope{{RoleCode: "settlement_finance_manager", ScopeType: "ORG_UNIT", ScopeID: "org-1"}},
	}
}

// tenantManager 返回同能力的租户级主体，范围覆盖全部单据。
// 参数：permission 为写操作能力码。
// 返回值：可执行全租户财务操作的主体。
func tenantManager(permission string) Principal {
	principal := scopedManager(permission)
	principal.DataScopes = []DataScope{{RoleCode: "settlement_finance_manager", ScopeType: "TENANT"}}
	return principal
}

// scopeSteps 返回一次责任范围解析固定产生的两条快照状态查询。
// 参数：state 为迁移状态（HOLD 时额外失败关闭）。
// 返回值：脚本步骤。
func scopeSteps(state driver.Value) []scriptdb.Step {
	return []scriptdb.Step{
		{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{state}}},
		{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{state}}},
	}
}

// TestApproveInvoiceRequestRejectsOutOfScopeManager 验证受限财务经理不能审批范围外申请。
// 参数：t 测试上下文。
// 返回值：无。
func TestApproveInvoiceRequestRejectsOutOfScopeManager(t *testing.T) {
	steps := []scriptdb.Step{
		{Kind: "begin"},
		{Kind: "query", SQL: "SELECT submitted_by", Rows: [][]driver.Value{{"applicant"}}},
		{Kind: "query", SQL: "SELECT contract_snapshot_id FROM settlement_invoice_request", Rows: [][]driver.Value{{"snapshot-out-of-scope"}}},
	}
	steps = append(steps, scopeSteps("RELEASED")...)
	steps = append(steps, scriptdb.Step{Kind: "rollback"})
	db := scriptdb.Open(t, steps...)
	_, err := (&Service{DB: db}).ApproveInvoiceRequest(t.Context(), scopedManager("settlement.invoice.approve"), "request-1", 1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope approval must be refused as not found: %v", err)
	}
}

// TestApproveInvoiceRequestAllowsInScopeManager 验证范围覆盖单据时审批仍正常流转到加签。
// 参数：t 测试上下文。
// 返回值：无。
func TestApproveInvoiceRequestAllowsInScopeManager(t *testing.T) {
	steps := []scriptdb.Step{
		{Kind: "begin"},
		{Kind: "query", SQL: "SELECT submitted_by", Rows: [][]driver.Value{{"applicant"}}},
		{Kind: "query", SQL: "SELECT contract_snapshot_id FROM settlement_invoice_request", Rows: [][]driver.Value{{"snapshot-1"}}},
	}
	steps = append(steps, scopeSteps("RELEASED")...)
	steps = append(steps,
		scriptdb.Step{Kind: "query", SQL: "SELECT required_review", Rows: [][]driver.Value{{true}}},
		scriptdb.Step{Kind: "exec", SQL: "status='SPECIAL_REVIEW'"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "commit"},
	)
	db := scriptdb.Open(t, steps...)
	attempt, err := (&Service{DB: db}).ApproveInvoiceRequest(t.Context(), tenantManager("settlement.invoice.approve"), "request-1", 1)
	if err != nil || attempt != "" {
		t.Fatalf("in-scope approval must still succeed: attempt=%q err=%v", attempt, err)
	}
}

// TestApproveInvoiceRequestRejectsHeldImportSnapshot 验证导入暂存合同即使范围可见也禁止审批。
// 参数：t 测试上下文。
// 返回值：无。
func TestApproveInvoiceRequestRejectsHeldImportSnapshot(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT submitted_by", Rows: [][]driver.Value{{"applicant"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_snapshot_id FROM settlement_invoice_request", Rows: [][]driver.Value{{"snapshot-hold"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"HOLD"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	_, err := (&Service{DB: db}).ApproveInvoiceRequest(t.Context(), tenantManager("settlement.invoice.approve"), "request-1", 1)
	if !errors.Is(err, ErrImportBlocked) {
		t.Fatalf("held import snapshot must block approval: %v", err)
	}
}

// TestApproveInvoiceRequestRejectsSalesRoleWithoutScopePredicate 验证销售角色即使被直接授予审批能力也不能越范围审批。
// 参数：t 测试上下文。
// 返回值：无。
func TestApproveInvoiceRequestRejectsSalesRoleWithoutScopePredicate(t *testing.T) {
	steps := []scriptdb.Step{
		{Kind: "begin"},
		{Kind: "query", SQL: "SELECT submitted_by", Rows: [][]driver.Value{{"applicant"}}},
		{Kind: "query", SQL: "SELECT contract_snapshot_id FROM settlement_invoice_request", Rows: [][]driver.Value{{"snapshot-1"}}},
	}
	steps = append(steps, scopeSteps("RELEASED")...)
	steps = append(steps, scriptdb.Step{Kind: "rollback"})
	db := scriptdb.Open(t, steps...)
	sales := Principal{
		TenantID: "tenant", UserID: "sales-1", CatalogVersion: "catalog-test",
		Permissions: map[string]bool{"settlement.invoice.approve": true},
		Roles:       []string{"settlement_sales"},
		DataScopes:  []DataScope{{RoleCode: "settlement_sales", ScopeType: "ORG_UNIT", ScopeID: "org-1"}},
	}
	if _, err := (&Service{DB: db}).ApproveInvoiceRequest(t.Context(), sales, "request-1", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sales role must not approve without a scope predicate: %v", err)
	}
}

// TestRejectInvoiceRequestRejectsOutOfScopeManager 验证范围外不得驳回并释放预留。
// 参数：t 测试上下文。
// 返回值：无。
func TestRejectInvoiceRequestRejectsOutOfScopeManager(t *testing.T) {
	steps := []scriptdb.Step{
		{Kind: "begin"},
		{Kind: "query", SQL: "SELECT submitted_by", Rows: [][]driver.Value{{"applicant"}}},
		{Kind: "query", SQL: "SELECT contract_snapshot_id FROM settlement_invoice_request", Rows: [][]driver.Value{{"snapshot-out-of-scope"}}},
	}
	steps = append(steps, scopeSteps("RELEASED")...)
	steps = append(steps, scriptdb.Step{Kind: "rollback"})
	db := scriptdb.Open(t, steps...)
	if err := (&Service{DB: db}).RejectInvoiceRequest(t.Context(), scopedManager("settlement.invoice.approve"), "request-1", 1, "资料不全"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope rejection must be refused as not found: %v", err)
	}
}

// TestReverseAllocationRejectsOutOfScopeManager 验证范围外不得冲销核销记录。
// 参数：t 测试上下文。
// 返回值：无。
func TestReverseAllocationRejectsOutOfScopeManager(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_receipt_allocation a WHERE a.id", Rows: [][]driver.Value{{"receipt-1", "receivable-1", "CNY", "confirmer", "100.00", "0.00"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT rv.contract_snapshot_id FROM settlement_receipt_allocation", Rows: [][]driver.Value{{"snapshot-out-of-scope"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	_, err := (&Service{DB: db}).ReverseAllocation(t.Context(), scopedManager("settlement.allocation.reverse"), "reverse-key", "allocation-1", ReversalInput{Amount: "10.00", ReasonCode: "E2E"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope reversal must be refused as not found: %v", err)
	}
}

// TestRegisterManualInvoiceRejectsOutOfScopeManager 验证范围外不得人工开票关单。
// 参数：t 测试上下文。
// 返回值：无。
func TestRegisterManualInvoiceRejectsOutOfScopeManager(t *testing.T) {
	steps := []scriptdb.Step{
		{Kind: "begin"},
		{Kind: "query", SQL: "FROM settlement_invoice_request ir JOIN settlement_contract_snapshot cs", Rows: [][]driver.Value{{"VAT_NORMAL", "CNY", "100.00", "13.00", "113.00", []byte(`{}`)}}},
		{Kind: "query", SQL: "SELECT contract_snapshot_id FROM settlement_invoice_request", Rows: [][]driver.Value{{"snapshot-out-of-scope"}}},
	}
	steps = append(steps, scopeSteps("RELEASED")...)
	steps = append(steps, scriptdb.Step{Kind: "rollback"})
	db := scriptdb.Open(t, steps...)
	_, err := (&Service{DB: db}).RegisterManualInvoice(t.Context(), scopedManager("settlement.invoice.issue"), "request-1", ManualInvoiceInput{InvoiceCode: "CODE", InvoiceNo: "NO-1", IssueDate: "2026-10-30"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope manual issue must be refused as not found: %v", err)
	}
}

// TestReviewInvoiceRedFlushRejectsOutOfScopeManager 验证范围外不得复核红冲申请。
// 参数：t 测试上下文。
// 返回值：无。
func TestReviewInvoiceRedFlushRejectsOutOfScopeManager(t *testing.T) {
	steps := []scriptdb.Step{
		{Kind: "begin"},
		{Kind: "query", SQL: "SELECT original_invoice_id,reason_code,reason_detail,requested_by", Rows: [][]driver.Value{{"invoice-1", "INVOICE_ERROR", "税额错误", "requester", "", "", "SUBMITTED", int64(1)}}},
		{Kind: "query", SQL: "SELECT ir.contract_snapshot_id FROM settlement_tax_invoice ti", Rows: [][]driver.Value{{"snapshot-out-of-scope"}}},
	}
	steps = append(steps, scopeSteps("RELEASED")...)
	steps = append(steps, scriptdb.Step{Kind: "rollback"})
	db := scriptdb.Open(t, steps...)
	err := (&Service{DB: db}).ReviewInvoiceRedFlush(t.Context(), scopedManager("settlement.invoice.approve"), "red-request-1", true, InvoiceRedFlushReviewInput{Version: 1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope red flush review must be refused as not found: %v", err)
	}
}

// TestRequestInvoiceRedFlushRejectsOutOfScopeManager 验证红冲申请同样按发票关系校验范围。
// 参数：t 测试上下文。
// 返回值：无。
func TestRequestInvoiceRedFlushRejectsOutOfScopeManager(t *testing.T) {
	steps := []scriptdb.Step{
		{Kind: "begin"},
		{Kind: "query", SQL: "SELECT id,original_invoice_id FROM settlement_invoice_red_flush_request"},
		{Kind: "query", SQL: "SELECT ti.status,ir.contract_snapshot_id", Rows: [][]driver.Value{{"ISSUED", "snapshot-out-of-scope"}}},
	}
	steps = append(steps, scopeSteps("RELEASED")...)
	steps = append(steps, scriptdb.Step{Kind: "rollback"})
	db := scriptdb.Open(t, steps...)
	if _, err := (&Service{DB: db}).RequestInvoiceRedFlush(t.Context(), scopedManager("settlement.invoice.issue"), "red-key", "invoice-1", InvoiceRedFlushInput{ReasonCode: "INVOICE_ERROR", ReasonDetail: "税额错误"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope red flush request must be refused as not found: %v", err)
	}
}

// TestRejectInvoiceRequestKeepsUnversionedPrincipalBehaviour 验证未版本化组件主体行为不变。
// 参数：t 测试上下文。
// 返回值：无。
func TestRejectInvoiceRequestKeepsUnversionedPrincipalBehaviour(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT submitted_by", Rows: [][]driver.Value{{"applicant"}}},
		scriptdb.Step{Kind: "exec", SQL: "status='REJECTED'"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_request_allocation SET released_amount"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "commit"},
	)
	if err := (&Service{DB: db}).RejectInvoiceRequest(t.Context(), Principal{TenantID: "tenant", UserID: "reviewer"}, "request-1", 1, "资料不全"); err != nil {
		t.Fatalf("unversioned component principal keeps existing behaviour: %v", err)
	}
}
