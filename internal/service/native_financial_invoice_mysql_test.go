package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/j-s-te/settlement/authz"
)

// nativeInvoicePrincipal 从正式嵌入目录复制单一角色的全部真实权限。
// 参数：t为测试上下文，user为不同操作人，role为已有职责编码。
// 返回值：同一隔离租户的版本化TENANT主体；目录或角色缺失时失败。
func nativeInvoicePrincipal(t *testing.T, user, role string) Principal {
	t.Helper()
	var manifest struct {
		CatalogVersion string `json:"catalog_version"`
		Roles          []struct {
			Code        string   `json:"code"`
			Permissions []string `json:"permissions"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(authz.PermissionManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, item := range manifest.Roles {
		if item.Code != role {
			continue
		}
		p := Principal{TenantID: "native-invoice-tenant", UserID: user,
			Roles: []string{role}, CatalogVersion: manifest.CatalogVersion,
			DataScopes:  []DataScope{{RoleCode: role, ScopeType: "TENANT"}},
			Permissions: map[string]bool{}}
		for _, permission := range item.Permissions {
			p.Permissions[permission] = true
		}
		if p.CatalogVersion == "" {
			t.Fatal("embedded catalog has no version")
		}
		return p
	}
	t.Fatalf("role %s absent from actual catalog", role)
	return Principal{}
}

// nativeInvoiceReceivable 用真实本地客户、依据和确认方法生成唯一100元应收。
// 参数：t/ctx为测试边界，s为隔离服务，manager为真实财务经理。
// 返回值：本地依据及应收ID；不插入合同、责任或CRM占位记录。
func nativeInvoiceReceivable(t *testing.T, ctx context.Context, s *Service, manager Principal) (NativeFinancialBaseline, string) {
	t.Helper()
	customer, err := s.CreateNativeFinancialCustomer(ctx, manager, "invoice-customer",
		NativeCustomerInput{Name: "QA原生财务开票客户"})
	if err != nil {
		t.Fatal(err)
	}
	input := nativeTestInput()
	input.CustomerKey = customer.CustomerKey
	input.PaymentTerms = []PaymentTerm{{InstallmentNo: 1, DueDate: "2026-11-05", Amount: "100.00"}}
	baseline, err := s.CreateNativeFinancialBaseline(ctx, manager, "invoice-baseline", input)
	if err != nil {
		t.Fatal(err)
	}
	var plan, receivable string
	var version int
	if err := s.DB.QueryRowContext(ctx, `SELECT id,version FROM settlement_receivable_plan
WHERE tenant_id=? AND contract_snapshot_id=?`, manager.TenantID, baseline.ID).Scan(&plan, &version); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmPlan(ctx, manager, plan, PlanConfirmationInput{Version: version}); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM settlement_receivable
WHERE tenant_id=? AND contract_snapshot_id=?`, manager.TenantID, baseline.ID).Scan(&receivable); err != nil {
		t.Fatal(err)
	}
	return baseline, receivable
}

// nativeInvoiceApproval 使用真实申请与两级审批消费原生快照及非数字客户引用。
// 参数：t/ctx/s为隔离边界，baseline/receivable为真实业务生成的账务依据。
// 返回值：由既有MANUAL方法登记的税票ID；不调用或伪造税控厂商回执。
func nativeInvoiceApproval(t *testing.T, ctx context.Context, s *Service, baseline, receivable string) string {
	t.Helper()
	applicant := nativeInvoicePrincipal(t, "accountant-applicant", "settlement_accountant")
	reviewer := nativeInvoicePrincipal(t, "independent-reviewer", "settlement_reviewer")
	director := nativeInvoicePrincipal(t, "independent-director", "settlement_sales_director")
	input := InvoiceRequestInput{ContractSnapshotID: baseline, InvoiceType: "VAT_NORMAL",
		BuyerProfile:  map[string]any{"name": "QA原生财务开票客户"},
		SpecialReason: "QA独立依据按既有规则加签",
		Items: []InvoiceItemInput{{ItemName: "QA服务", Quantity: "1", UnitPriceExclTax: "100.00",
			AmountExclTax: "100.00", TaxRate: "0", TaxAmount: "0.00", AmountInclTax: "100.00"}},
		Allocations: []InvoiceAllocationInput{{ReceivableID: receivable, ReservedAmount: "100.00"}}}
	request, err := s.CreateInvoiceRequest(ctx, applicant, "native-invoice-request", input)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := s.CreateInvoiceRequest(ctx, applicant, "native-invoice-request", input); err != nil || replay != request {
		t.Fatalf("request replay changed identity: %q %v", replay, err)
	}
	if _, err := s.ApproveInvoiceRequest(ctx, applicant, request, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("applicant reviewed own invoice: %v", err)
	}
	if _, err := s.RegisterManualInvoice(ctx, applicant, request,
		ManualInvoiceInput{InvoiceCode: "QA-NATIVE", InvoiceNo: "QA-001", IssueDate: "2026-11-04"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("manual registration bypassed approvals: %v", err)
	}
	if attempt, err := s.ApproveInvoiceRequest(ctx, reviewer, request, 1); err != nil || attempt != "" {
		t.Fatalf("first approval skipped director: %q %v", attempt, err)
	}
	if _, err := s.ReviewSpecialInvoice(ctx, reviewer, request, 2, true, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reviewer acquired director capability: %v", err)
	}
	if attempt, err := s.ReviewSpecialInvoice(ctx, director, request, 2, true, ""); err != nil || attempt == "" {
		t.Fatalf("director approval failed: %q %v", attempt, err)
	}
	invoice, err := s.RegisterManualInvoice(ctx, applicant, request, ManualInvoiceInput{
		InvoiceCode: "QA-NATIVE", InvoiceNo: "QA-001", IssueDate: "2026-11-04",
		SellerProfile: map[string]any{"name": "QA人工票据登记方"}})
	if err != nil {
		t.Fatal(err)
	}
	return invoice
}

// nativeInvoicePDF 构造结构完整的单页合成PDF，不引用真实业务票据。
// 参数：无。
// 返回值：带交叉引用表的PDF字节；未声称经过真实文件扫描或税控签发。
func nativeInvoicePDF() []byte {
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\nendstream",
	}
	offsets := []int{0}
	for i, object := range objects {
		offsets = append(offsets, output.Len())
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return output.Bytes()
}

// nativeInvoiceArchive 验证原归档事务、许可拒绝和同键重放，网关明确使用HTTP端口替身。
// 参数：t/ctx/s为隔离边界，invoice为审批后真实MANUAL登记的税票。
// 返回值：无；未确认许可、重复文件或缺失归档审计时失败。
func nativeInvoiceArchive(t *testing.T, ctx context.Context, s *Service, invoice string) {
	t.Helper()
	issuer := nativeInvoicePrincipal(t, "accountant-applicant", "settlement_accountant")
	boundary := &archiveHTTPBoundary{requests: map[string]string{}}
	gateway, identity := boundaryArchiveGateway(t, boundary)
	identity.TenantID = issuer.TenantID
	input := archiveInput(invoice, "native-invoice-archive")
	input.Content = nativeInvoicePDF()
	input.Checksum = fmt.Sprintf("%x", sha256.Sum256(input.Content))
	manager := nativeInvoicePrincipal(t, "finance-manager", "settlement_finance_manager")
	if _, _, err := s.UploadInvoiceDocument(ctx, manager, identity, gateway, input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager acquired invoice issue capability: %v", err)
	}
	denied := errors.New("synthetic commercial denial")
	s.CheckInvoiceArchiveLicense = func(context.Context) error { return denied }
	if _, _, err := s.UploadInvoiceDocument(ctx, issuer, identity, gateway, input); !errors.Is(err, ErrInvoiceArchiveLicense) {
		t.Fatalf("archive license denial lost: %v", err)
	}
	if len(boundary.requests) != 0 {
		t.Fatal("denied archive reached gateway substitute")
	}
	s.CheckInvoiceArchiveLicense = func(context.Context) error { return nil }
	document, created, err := s.UploadInvoiceDocument(ctx, issuer, identity, gateway, input)
	if err != nil || !created || document == "" {
		t.Fatalf("archive failed: document=%q created=%v err=%v", document, created, err)
	}
	if replay, created, err := s.UploadInvoiceDocument(ctx, issuer, identity, gateway, input); err != nil || created || replay != document {
		t.Fatalf("archive replay changed document: %q %v %v", replay, created, err)
	}
	var documents, audits int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_invoice_document
WHERE tenant_id=? AND tax_invoice_id=? AND id=? AND media_type='application/pdf'`, issuer.TenantID, invoice, document).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_outbox_event
WHERE tenant_id=? AND event_type='SETTLEMENT_INVOICE_DOCUMENT_ARCHIVED' AND aggregate_id=?`, issuer.TenantID, invoice).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if documents != 1 || audits != 1 || len(boundary.requests) != 1 || archiveState(t, s.DB, invoice) != "ARCHIVED" {
		t.Fatal("archive/replay did not preserve one committed document, upload and audit")
	}
}

// TestNativeFinancialInvoiceMySQL 验证原生依据经既有开票、分离审批和人工归档完整消费。
// 参数：t为测试上下文；仅复用严格SETTLEMENT_NATIVE_TEST_DSN新空库门禁。
// 返回值：无；未配置DSN明确跳过，NULL来源、权限或账务异常时失败。
func TestNativeFinancialInvoiceMySQL(t *testing.T) {
	db := nativeFinancialMySQL(t)
	s := &Service{DB: db, InvoiceIssuanceMode: "manual"}
	s.ConfigureFinancialSources(true, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := nativeInvoicePrincipal(t, "finance-manager", "settlement_finance_manager")
	baseline, receivable := nativeInvoiceReceivable(t, ctx, s, manager)
	invoice := nativeInvoiceApproval(t, ctx, s, baseline.ID, receivable)
	var source sql.NullString
	var basis, namespace, customer, status, amount, invoiced, open string
	if err := db.QueryRowContext(ctx, `SELECT cs.source_contract_id,cs.basis_type,
cs.customer_namespace,cs.customer_id,ti.status,ti.amount_incl_tax,r.invoiced_amount,r.open_amount
FROM settlement_tax_invoice ti
JOIN settlement_invoice_request ir ON ir.id=ti.invoice_request_id AND ir.tenant_id=ti.tenant_id
JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id
JOIN settlement_receivable r ON r.contract_snapshot_id=cs.id AND r.tenant_id=cs.tenant_id
WHERE ti.id=? AND ti.tenant_id=?`, invoice, manager.TenantID).Scan(
		&source, &basis, &namespace, &customer, &status, &amount, &invoiced, &open); err != nil {
		t.Fatal(err)
	}
	if source.Valid || basis != NativeFinancialBasis || namespace != "settlement_local" ||
		customer != baseline.CustomerKey || status != "ISSUED" || amount != "100.00" ||
		invoiced != "100.00" || open != "100.00" {
		t.Fatalf("native invoice metadata or ledger changed: source=%v basis=%q namespace=%q status=%q amount=%q invoiced=%q open=%q",
			source.Valid, basis, namespace, status, amount, invoiced, open)
	}
	nativeInvoiceArchive(t, ctx, s, invoice)
	for _, table := range []string{"settlement_contract_responsibility", "settlement_credit_sync", "settlement_inbox_event"} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE tenant_id=?", manager.TenantID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unexpected contract/credit/inbox fact in %s: count=%d err=%v", table, count, err)
		}
	}
}
