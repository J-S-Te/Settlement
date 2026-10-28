package service

import (
	"errors"
	"testing"
)

// TestMySQLRoleFinanceWorkflow 验证实际迁移、特殊两阶段审批及差异/凭证事务。
// 参数：t 测试上下文。
// 返回值：无。
func TestMySQLRoleFinanceWorkflow(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := t.Context()
	tenant := "tenant-role-finance"
	invoice := seedArchiveInvoice(t, db, tenant)
	var snapshot string
	if err := db.QueryRow(`SELECT ir.contract_snapshot_id FROM settlement_invoice_request ir JOIN settlement_tax_invoice ti ON ti.invoice_request_id=ir.id WHERE ti.id=?`, invoice).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE settlement_contract_snapshot SET contract_amount=100 WHERE id=?`, snapshot); err != nil {
		t.Fatal(err)
	}
	r := newID()
	if _, err := db.Exec(`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,due_date,original_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,?,?, ?,CURDATE(),100,100,'CNY','CONFIRMED','UNPAID','UNINVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, r, tenant, "R-"+r, snapshot); err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: db}
	sales := Principal{TenantID: tenant, UserID: "sales", Permissions: map[string]bool{"settlement.invoice.request": true, "settlement.invoice.special_approve": true}}
	req, err := svc.CreateInvoiceRequest(ctx, sales, "special-request", InvoiceRequestInput{SpecialReason: "特殊折扣说明", ContractSnapshotID: snapshot, InvoiceType: "VAT_NORMAL", Items: []InvoiceItemInput{{ItemName: "服务", Quantity: "1", UnitPriceExclTax: "10", AmountExclTax: "10", TaxRate: "0.13", TaxAmount: "1.30", AmountInclTax: "11.30"}}, Allocations: []InvoiceAllocationInput{{ReceivableID: r, ReservedAmount: "11.30"}}})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := svc.ApproveInvoiceRequest(ctx, Principal{TenantID: tenant, UserID: "finance"}, req, 1)
	if err != nil || attempt != "" {
		t.Fatalf("finance attempt=%s err=%v", attempt, err)
	}
	var attempts int
	if err = db.QueryRow(`SELECT COUNT(*) FROM settlement_invoice_issue_attempt WHERE invoice_request_id=?`, req).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("premature attempts=%d err=%v", attempts, err)
	}
	if _, err = svc.ReviewSpecialInvoice(ctx, sales, req, 2, true, ""); !errors.Is(err, ErrForbidden) {
		t.Fatal("self special review", err)
	}
	attempt, err = svc.ReviewSpecialInvoice(ctx, Principal{TenantID: tenant, UserID: "director", Permissions: map[string]bool{"settlement.invoice.special_approve": true}}, req, 2, true, "")
	if err != nil || attempt == "" {
		t.Fatalf("director attempt=%s err=%v", attempt, err)
	}
	receipt, allocation := newID(), newID()
	if _, err = db.Exec(`INSERT INTO settlement_receipt(id,tenant_id,receipt_no,customer_id,customer_name_snapshot,amount,unallocated_amount,currency,receipt_date,payment_method,bank_transaction_reference,source_type,status,idempotency_key,created_at,updated_at) VALUES(?,?,?,'customer','测试客户',100,90,'CNY',CURDATE(),'BANK',?,'MANUAL','CONFIRMED',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, receipt, tenant, receipt, receipt, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO settlement_receipt_allocation(id,tenant_id,allocation_no,receipt_id,receivable_id,allocated_amount,currency,status,match_mode,confirmed_by,confirmed_at,idempotency_key,created_at) VALUES(?,?,?,?,?,10,'CNY','CONFIRMED','MANUAL','accountant',UTC_TIMESTAMP(3),?,UTC_TIMESTAMP(3))`, allocation, tenant, allocation, receipt, r, allocation); err != nil {
		t.Fatal(err)
	}
	accountant := Principal{TenantID: tenant, UserID: "accountant", Permissions: map[string]bool{"settlement.difference.manage": true, "settlement.voucher.manage": true}}
	in := ReceiptDifferenceInput{Key: "difference-key", ReceiptID: receipt, Kind: "UNKNOWN", Amount: "2", Reason: "不明款待核实"}
	difference, err := svc.CreateReceiptDifference(ctx, accountant, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.CreateReceiptDifference(ctx, accountant, in)
	if err != nil || again != difference {
		t.Fatalf("idempotent difference=%s err=%v", again, err)
	}
	var balance string
	if err = db.QueryRow(`SELECT unallocated_amount FROM settlement_receipt WHERE id=?`, receipt).Scan(&balance); err != nil || balance != "88.00" {
		t.Fatalf("held balance=%s err=%v", balance, err)
	}
	manager := Principal{TenantID: tenant, UserID: "manager", Permissions: map[string]bool{"settlement.difference.manage": true, "settlement.difference.approve": true}}
	if err = svc.ResolveReceiptDifference(ctx, manager, difference, DifferenceResolutionInput{Version: 1, Resolution: "CARRY_FORWARD", Reason: "已查明转后续核销"}); err != nil {
		t.Fatal(err)
	}
	if err = svc.ApproveReceiptDifference(ctx, manager, difference, 2); !errors.Is(err, ErrForbidden) {
		t.Fatal("self resolution approved", err)
	}
	manager.UserID = "other-manager"
	if err = svc.ApproveReceiptDifference(ctx, manager, difference, 2); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT unallocated_amount FROM settlement_receipt WHERE id=?`, receipt).Scan(&balance); err != nil || balance != "90.00" {
		t.Fatalf("released balance=%s err=%v", balance, err)
	}
	voucher, err := svc.CreateAccountingVoucher(ctx, accountant, allocation)
	if err != nil {
		t.Fatal(err)
	}
	state, err := svc.RetryAccountingVoucher(ctx, accountant, voucher)
	if err != nil || state != "NOT_CONFIGURED" {
		t.Fatalf("state %s err %v", state, err)
	}
	svc.ERPProvider = rejectedERP{}
	state, err = svc.RetryAccountingVoucher(ctx, accountant, voucher)
	if err != nil || state != "FAILED" {
		t.Fatalf("state %s err %v", state, err)
	}
}
