package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/businessclock"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// invoiceRequestFixture 返回一份通过既有要素校验的开票申请输入，
// 供买方档案/发票类型校验用例复用（避免每个用例重复金额口径）。
// 参数：buyerProfile 为买方档案，invoiceType 为发票类型。
// 返回值：可直接提交给 CreateInvoiceRequest 的输入。
func invoiceRequestFixture(buyerProfile map[string]any, invoiceType string) InvoiceRequestInput {
	return InvoiceRequestInput{
		ContractSnapshotID: "snapshot-1",
		BuyerProfile:       buyerProfile,
		InvoiceType:        invoiceType,
		Items: []InvoiceItemInput{{
			ItemName: "服务", TaxClassificationCode: "CLASS-1", Unit: "项", Quantity: "1",
			UnitPriceExclTax: "10.00", AmountExclTax: "10.00", TaxRate: "0.13", TaxAmount: "1.30", AmountInclTax: "11.30",
		}},
		Allocations: []InvoiceAllocationInput{{ReceivableID: "receivable-1", ReservedAmount: "11.30"}},
	}
}

// TestBuyerProfileWhitelist 验证买方档案只接受白名单字段、字符串值且不超长度上限。
// 返回 ErrInvalid 表示客户端伪造的字段/超长值在入库前即被拒绝。
func TestBuyerProfileWhitelist(t *testing.T) {
	cases := []struct {
		name    string
		profile map[string]any
		wantErr bool
	}{
		{name: "现有前端合法载荷", profile: map[string]any{"customer_id": "customer-1", "name": "示例客户", "tax_no": "91310000MA1K3XYZ12"}},
		{name: "完整抬头要素", profile: map[string]any{"name": "示例客户", "tax_no": "91", "address": "上海市浦东新区", "phone": "021-00000000", "bank_name": "示例银行", "bank_account": "6222000000000000"}},
		{name: "空档案", profile: map[string]any{}},
		// 关键：未知键不得原样入库并透传税控。
		{name: "未知键", profile: map[string]any{"name": "示例客户", "settlement_admin": "true"}, wantErr: true},
		{name: "未知键_税号拼音", profile: map[string]any{"taxNo": "91"}, wantErr: true},
		{name: "超长税号", profile: map[string]any{"tax_no": strings.Repeat("9", 65)}, wantErr: true},
		{name: "超长名称", profile: map[string]any{"name": strings.Repeat("客", 256)}, wantErr: true},
		{name: "超长客户标识", profile: map[string]any{"customer_id": strings.Repeat("c", 129)}, wantErr: true},
		// 非字符串值会被下游税控/展示按结构体解码吞掉，必须直接拒绝。
		{name: "非字符串值", profile: map[string]any{"tax_no": 91310000}, wantErr: true},
		{name: "嵌套对象", profile: map[string]any{"name": map[string]any{"v": "x"}}, wantErr: true},
		{name: "控制字符", profile: map[string]any{"name": "示例\n客户"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			normalized, err := normalizeInvoiceProfile(tc.profile, buyerProfileFields, "buyer_profile")
			if tc.wantErr {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("err = %v; want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("合法档案被拒绝: %v", err)
			}
			if normalized == nil {
				t.Fatal("归一化结果不应为 nil")
			}
			// 归一化后只保留白名单键，且去掉首尾空白。
			for key := range normalized {
				if _, ok := buyerProfileFields[key]; !ok {
					t.Fatalf("归一化结果保留了白名单外键 %q", key)
				}
			}
		})
	}
	// 首尾空白在归一化时去掉，避免同一抬头因空格产生不同快照与幂等哈希。
	trimmed, err := normalizeInvoiceProfile(map[string]any{"tax_no": "  91310000MA1K3XYZ12  "}, buyerProfileFields, "buyer_profile")
	if err != nil {
		t.Fatalf("去空白用例失败: %v", err)
	}
	if trimmed["tax_no"] != "91310000MA1K3XYZ12" {
		t.Fatalf("tax_no = %v; want 去空白后的税号", trimmed["tax_no"])
	}
}

// TestSellerProfileWhitelist 验证销售方档案同样按白名单受控，且不接受买方专属字段。
func TestSellerProfileWhitelist(t *testing.T) {
	if _, err := normalizeInvoiceProfile(map[string]any{"name": "开票方", "tax_no": "91310000MA1K3XYZ12", "bank_account": "6222"}, sellerProfileFields, "seller_profile"); err != nil {
		t.Fatalf("合法销售方档案被拒绝: %v", err)
	}
	// 前端人工登记当前发送空对象，必须继续通过。
	if profile, err := normalizeInvoiceProfile(nil, sellerProfileFields, "seller_profile"); err != nil || len(profile) != 0 {
		t.Fatalf("空销售方档案应按空对象处理, profile=%v err=%v", profile, err)
	}
	for _, profile := range []map[string]any{
		{"name": "开票方", "unexpected": "x"},
		{"customer_id": "customer-1"},
		{"name": strings.Repeat("开", 256)},
	} {
		if _, err := normalizeInvoiceProfile(profile, sellerProfileFields, "seller_profile"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("profile=%v err = %v; want ErrInvalid", profile, err)
		}
	}
}

// TestInvoiceTypeWhitelist 验证发票类型枚举，未知取值与超长值均被拒绝。
func TestInvoiceTypeWhitelist(t *testing.T) {
	cases := []struct {
		value   string
		want    string
		wantErr bool
	}{
		{value: "ELECTRONIC_NORMAL", want: InvoiceTypeElectronicNormal},
		{value: "electronic_special", want: InvoiceTypeElectronicSpecial},
		{value: " PAPER_NORMAL ", want: InvoiceTypePaperNormal},
		{value: "PAPER_SPECIAL", want: InvoiceTypePaperSpecial},
		{value: "", wantErr: true},
		{value: "   ", wantErr: true},
		{value: "GIFT", wantErr: true},
		{value: "RED_FLUSH", wantErr: true},
		{value: strings.Repeat("A", 33), wantErr: true},
	}
	for _, tc := range cases {
		got, err := normalizeInvoiceType(tc.value)
		if tc.wantErr {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("value=%q err = %v; want ErrInvalid", tc.value, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("value=%q got=(%q,%v); want %q", tc.value, got, err, tc.want)
		}
	}
}

// TestBuyerProfileCustomerConsistency 验证买方客户标识必须与合同快照客户一致。
func TestBuyerProfileCustomerConsistency(t *testing.T) {
	if err := requireBuyerProfileCustomer(map[string]any{"customer_id": "customer-1"}, "customer-1"); err != nil {
		t.Fatalf("一致客户被拒绝: %v", err)
	}
	// 未提供或空串等同于不声明客户，不额外放大服务端判定。
	for _, profile := range []map[string]any{{}, {"name": "示例客户"}, {"customer_id": ""}} {
		if err := requireBuyerProfileCustomer(profile, "customer-1"); err != nil {
			t.Fatalf("profile=%v err = %v; want nil", profile, err)
		}
	}
	if err := requireBuyerProfileCustomer(map[string]any{"customer_id": "customer-2"}, "customer-1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("不一致客户 err = %v; want ErrInvalid", err)
	}
}

// TestInvoiceDocumentNumberWhitelist 验证票号长度与字符白名单。
func TestInvoiceDocumentNumberWhitelist(t *testing.T) {
	for _, value := range []string{"0073123456", "CODE-1", "abc123"} {
		if err := validateInvoiceDocumentNumber(value, "invoice_no"); err != nil {
			t.Fatalf("合法票号 %q 被拒绝: %v", value, err)
		}
	}
	cases := []string{
		"",
		strings.Repeat("9", 33),
		"12A/34",
		"12 34",
		"１２３", // 全角数字不在白名单
		"NO;DROP",
		"NO'1",
	}
	for _, value := range cases {
		if err := validateInvoiceDocumentNumber(value, "invoice_no"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("value=%q err = %v; want ErrInvalid", value, err)
		}
	}
}

// TestInvoiceIssueDateBusinessDayBound 验证开票日期不晚于北京业务日，
// 且比较基准是业务日而不是 UTC 当前时刻。
func TestInvoiceIssueDateBusinessDayBound(t *testing.T) {
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC) // 北京 2026-10-20
	for _, value := range []string{"2026-10-20", "2026-10-19", "2020-01-01"} {
		if got, err := validateInvoiceIssueDateOn(value, today); err != nil || got != value {
			t.Fatalf("value=%q got=(%q,%v); want 通过", value, got, err)
		}
	}
	for _, value := range []string{"2026-10-21", "2027-01-01"} {
		if _, err := validateInvoiceIssueDateOn(value, today); !errors.Is(err, ErrInvalid) {
			t.Fatalf("value=%q err = %v; want ErrInvalid", value, err)
		}
	}
	for _, value := range []string{"", "2026-1-1", "2026-13-01", "2026/10/20", "2026-10-20T00:00:00Z"} {
		if _, err := validateInvoiceIssueDateOn(value, today); !errors.Is(err, ErrInvalid) {
			t.Fatalf("value=%q err = %v; want ErrInvalid", value, err)
		}
	}
	// 生产入口按北京业务日判定：明天必被拒绝，昨天必被接受。
	if _, err := validateInvoiceIssueDate(businessclock.DateString(businessclock.Today().AddDate(0, 0, 2))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("未来业务日 err = %v; want ErrInvalid", err)
	}
	if _, err := validateInvoiceIssueDate(businessclock.DateString(businessclock.Today().AddDate(0, 0, -1))); err != nil {
		t.Fatalf("过去业务日 err = %v; want nil", err)
	}
}

// TestCreateInvoiceRequestRejectsForgedInputBeforeTransaction 验证伪造要素在开启事务前即被拒绝，
// 因此不需要数据库（Service{DB:nil} 也不应被触碰）。
func TestCreateInvoiceRequestRejectsForgedInputBeforeTransaction(t *testing.T) {
	service := &Service{}
	p := Principal{TenantID: "tenant", UserID: "user"}
	cases := []struct {
		name string
		in   InvoiceRequestInput
	}{
		{name: "未知买方键", in: invoiceRequestFixture(map[string]any{"customer_id": "customer-1", "extra": "x"}, InvoiceTypeElectronicNormal)},
		{name: "超长买方税号", in: invoiceRequestFixture(map[string]any{"tax_no": strings.Repeat("9", 65)}, InvoiceTypeElectronicNormal)},
		{name: "非法发票类型", in: invoiceRequestFixture(map[string]any{"customer_id": "customer-1"}, "GIFT")},
		{name: "空发票类型", in: invoiceRequestFixture(map[string]any{"customer_id": "customer-1"}, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.CreateInvoiceRequest(context.Background(), p, "key", tc.in); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v; want ErrInvalid", err)
			}
		})
	}
}

// TestCreateInvoiceRequestRejectsForgedBuyerCustomer 验证买方档案声明的客户
// 与合同快照客户不一致时在事务内被拒绝（返回校验错误而非静默写入）。
func TestCreateInvoiceRequestRejectsForgedBuyerCustomer(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		// 合同快照客户为 customer-snapshot，买方档案声明 customer-other。
		scriptdb.Step{Kind: "query", SQL: "SELECT id,customer_id FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"snapshot-1", "customer-snapshot"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	p := Principal{TenantID: "tenant", UserID: "user", Permissions: map[string]bool{"settlement.invoice.request": true}}
	_, err := (&Service{DB: db}).CreateInvoiceRequest(context.Background(), p, "key", invoiceRequestFixture(map[string]any{"customer_id": "customer-other"}, InvoiceTypeElectronicNormal))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v; want ErrInvalid", err)
	}
}

// TestCreateInvoiceRequestAcceptsWhitelistedBuyerProfile 验证合法买方档案与发票类型
// 能通过客户端校验与客户一致性检查，继续进入既有事务路径。
func TestCreateInvoiceRequestAcceptsWhitelistedBuyerProfile(t *testing.T) {
	reached := errors.New("reached allocation lookup")
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id,customer_id FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"snapshot-1", "customer-1"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT original_amount,invoiced_amount FROM settlement_receivable", Err: reached},
		scriptdb.Step{Kind: "rollback"},
	)
	p := Principal{TenantID: "tenant", UserID: "user", Permissions: map[string]bool{"settlement.invoice.request": true}}
	_, err := (&Service{DB: db}).CreateInvoiceRequest(context.Background(), p, "key", invoiceRequestFixture(map[string]any{"customer_id": "customer-1", "name": "示例客户", "tax_no": "91310000MA1K3XYZ12"}, InvoiceTypeElectronicNormal))
	// 未被校验拦下：错误来自后续额度查询，说明合法要素仍走原有成功路径。
	if !errors.Is(err, reached) {
		t.Fatalf("err = %v; want 事务内额度查询错误", err)
	}
}

// TestRegisterManualInvoiceRejectsForgedElements 验证人工登记票号/日期/销售方档案的伪造输入被拒绝。
func TestRegisterManualInvoiceRejectsForgedElements(t *testing.T) {
	service := &Service{}
	p := Principal{TenantID: "tenant", UserID: "user"}
	future := businessclock.DateString(businessclock.Today().AddDate(0, 0, 2))
	cases := []struct {
		name string
		in   ManualInvoiceInput
	}{
		{name: "未来开票日期", in: ManualInvoiceInput{InvoiceCode: "0073123456", InvoiceNo: "00000001", IssueDate: future}},
		{name: "非法日期格式", in: ManualInvoiceInput{InvoiceCode: "0073123456", InvoiceNo: "00000001", IssueDate: "2026-1-1"}},
		{name: "超长票号", in: ManualInvoiceInput{InvoiceCode: "0073123456", InvoiceNo: strings.Repeat("9", 33), IssueDate: "2026-10-20"}},
		{name: "非法字符票号", in: ManualInvoiceInput{InvoiceCode: "0073/123", InvoiceNo: "00000001", IssueDate: "2026-10-20"}},
		{name: "未知销售方键", in: ManualInvoiceInput{InvoiceCode: "0073123456", InvoiceNo: "00000001", IssueDate: "2026-10-20", SellerProfile: map[string]any{"name": "开票方", "tenant_admin": "true"}}},
		{name: "超长销售方名称", in: ManualInvoiceInput{InvoiceCode: "0073123456", InvoiceNo: "00000001", IssueDate: "2026-10-20", SellerProfile: map[string]any{"name": strings.Repeat("开", 256)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.RegisterManualInvoice(context.Background(), p, "request-1", tc.in); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v; want ErrInvalid", err)
			}
		})
	}
}

// TestRegisterManualInvoiceLegalInputEntersTransaction 验证合法人工登记要素通过校验并进入事务。
func TestRegisterManualInvoiceLegalInputEntersTransaction(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		// 申请查询失败按既有契约返回冲突，而不是校验错误。
		scriptdb.Step{Kind: "query", SQL: "SELECT ir.invoice_type,cs.currency", Err: errors.New("undefined table")},
		scriptdb.Step{Kind: "rollback"},
	)
	p := Principal{TenantID: "tenant", UserID: "user"}
	in := ManualInvoiceInput{InvoiceCode: "0073123456", InvoiceNo: "00000001", IssueDate: businessclock.DateString(businessclock.Today()), SellerProfile: map[string]any{"name": "开票方", "tax_no": "91310000MA1K3XYZ12"}}
	_, err := (&Service{DB: db}).RegisterManualInvoice(context.Background(), p, "request-1", in)
	if errors.Is(err, ErrInvalid) {
		t.Fatalf("合法人工登记被校验拒绝: %v", err)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v; want ErrConflict", err)
	}
}

// TestStoredInvoiceTypeShape 验证出站税控命令对库内发票类型只做形态收口：
// 历史合法取值放行，任意文本/超长值拒绝。
func TestStoredInvoiceTypeShape(t *testing.T) {
	for _, value := range []string{"ELECTRONIC_NORMAL", "VAT_SPECIAL", "SPECIAL", "paper_normal"} {
		if _, err := validateStoredInvoiceType(value); err != nil {
			t.Fatalf("库内合法取值 %q 被拒绝: %v", value, err)
		}
	}
	for _, value := range []string{"", "   ", "电子普通发票", "ELECTRONIC NORMAL", "ELECTRONIC;DROP", strings.Repeat("A", 33)} {
		if _, err := validateStoredInvoiceType(value); !errors.Is(err, ErrInvalid) {
			t.Fatalf("value=%q err = %v; want ErrInvalid", value, err)
		}
	}
}
