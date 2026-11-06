package service

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

const nativeTestCustomerID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// nativeTestPrincipal 构造已有目录中的租户财务经理，不新增角色或授权。
// 参数：role 为测试的真实角色编码。
// 返回值：携带可信范围元数据的主体。
func nativeTestPrincipal(role string) Principal {
	return Principal{TenantID: "tenant-a", UserID: "finance-a", Roles: []string{role}, CatalogVersion: "v3",
		Permissions: map[string]bool{"settlement.receivable.create": true, "settlement.receivable.read": true},
		DataScopes:  []DataScope{{RoleCode: role, ScopeType: "TENANT"}}}
}

// TestNativeBaselineReplayReturnsAuthoritativeState 验证创建幂等重放保留原ID与现态且不额外要求读取授权。
// 参数：t 为测试上下文。
// 返回值：无；重复插入或权限漂移导致重放失败时失败。
func TestNativeBaselineReplayReturnsAuthoritativeState(t *testing.T) {
	p := nativeTestPrincipal("settlement_finance_manager")
	delete(p.Permissions, "settlement.receivable.read")
	input, err := normalizeNativeFinancial(nativeTestInput(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(struct {
		Actor string
		Input NativeFinancialInput
	}{p.UserID, input})
	hash := sha256.Sum256(body)
	row := []driver.Value{"snapshot-1", NativeFinancialBasis, "snapshot-1", "FB-1", int64(2), input.CustomerKey, "本地客户", "CNY", "100.00", "2020-01-01", "VOID", "PURCHASE_ORDER", "PO-1", "原始依据", nil, "", "settlement_local"}
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT name FROM settlement_financial_customer", Rows: [][]driver.Value{{"本地客户"}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{hash[:], "snapshot-1"}}}, scriptdb.Step{Kind: "commit"},
		scriptdb.Step{Kind: "query", SQL: "AND basis_type='LOCAL_FINANCIAL' AND id=?", Rows: [][]driver.Value{row}})
	value, err := (&Service{DB: db, NativeFinancialEnabled: true}).CreateNativeFinancialBaseline(context.Background(), p, "same-key", input)
	if err != nil || value.ID != "snapshot-1" || value.Status != "VOID" {
		t.Fatalf("value=%+v err=%v", value, err)
	}
}

// TestNativeCustomerIdempotencyRejectsChangedPayload 验证同一键异内容不新建客户。
// 参数：t 为测试上下文。
// 返回值：无；冲突未回滚时失败。
func TestNativeCustomerIdempotencyRejectsChangedPayload(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{[]byte("other-payload"), nativeTestCustomerID}}}, scriptdb.Step{Kind: "rollback"})
	_, err := (&Service{DB: db, NativeFinancialEnabled: true}).CreateNativeFinancialCustomer(context.Background(), nativeTestPrincipal("settlement_finance_manager"), "same-key", NativeCustomerInput{Name: "changed"})
	if !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

// TestNativeVoidRejectsVersionTenantAndInvoiceConsumption 验证跨租户、旧版本与已占用开票不会作废。
// 参数：t 为测试上下文。
// 返回值：无；任意后续账务更新发生时失败。
func TestNativeVoidRejectsVersionTenantAndInvoiceConsumption(t *testing.T) {
	for _, kind := range []string{"foreign-tenant", "stale-version", "invoice-consumed"} {
		t.Run(kind, func(t *testing.T) {
			var args []driver.NamedValue
			step := scriptdb.Step{Kind: "query", SQL: "SELECT financial_status,local_document_version", Rows: [][]driver.Value{{"EFFECTIVE", int64(1)}}, Args: &args}
			want := ErrConflict
			if kind == "foreign-tenant" {
				step.Rows = nil
				step.Columns = 2
				want = ErrNotFound
			}
			if kind == "stale-version" {
				step.Rows = [][]driver.Value{{"EFFECTIVE", int64(2)}}
			}
			steps := []scriptdb.Step{{Kind: "begin"}, {Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"}, step}
			if kind == "invoice-consumed" {
				want = ErrNativeFinancialConsumed
				steps = append(steps, scriptdb.Step{Kind: "query", SQL: "SELECT confirmation_status", Rows: [][]driver.Value{{"PENDING_CONFIRMATION"}}}, scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_receivable", Columns: 1}, scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_invoice_request", Rows: [][]driver.Value{{"invoice"}}})
			}
			steps = append(steps, scriptdb.Step{Kind: "rollback"})
			err := (&Service{DB: scriptdb.Open(t, steps...), NativeFinancialEnabled: true}).VoidNativeFinancialBaseline(context.Background(), nativeTestPrincipal("settlement_finance_manager"), "snapshot", "key", NativeFinancialVoidInput{Version: 1, Reason: "作废重建"})
			if !errors.Is(err, want) || args[0].Value != "tenant-a" {
				t.Fatalf("err=%v args=%+v", err, args)
			}
		})
	}
}

// nativeTestInput 提供真实本地引用和精确分期的规范测试输入。
// 参数：无。
// 返回值：总额100且不含合同或CRM身份的财务依据。
func nativeTestInput() NativeFinancialInput {
	return NativeFinancialInput{CustomerKey: NativeCustomerPrefix + nativeTestCustomerID, Currency: "CNY", Amount: "100.00", EffectiveDate: "2020-01-01",
		EvidenceType: "PURCHASE_ORDER", EvidenceReference: "PO-1", EvidenceNote: "采购单由财务留存", Reason: "登记客户采购依据",
		PaymentTerms: []PaymentTerm{{InstallmentNo: 1, DueDate: "2020-02-01", Amount: "50.00"}, {InstallmentNo: 2, DueDate: "2020-03-01", Amount: "50.00"}}}
}

// TestNativeFinancialPermissionBeforeDatabase 验证默认关闭与已有角色范围均在SQL前拦截。
// 参数：t 为测试上下文。
// 返回值：无；无权限仍触库或扩大销售权限时失败。
func TestNativeFinancialPermissionBeforeDatabase(t *testing.T) {
	if _, err := (&Service{}).CreateNativeFinancialBaseline(context.Background(), nativeTestPrincipal("settlement_finance_manager"), "key", nativeTestInput()); !errors.Is(err, ErrNativeFinancialDisabled) {
		t.Fatal(err)
	}
	for _, role := range []string{"settlement_sales", "settlement_project_assistant", "settlement_accountant"} {
		if _, err := (&Service{NativeFinancialEnabled: true}).CreateNativeFinancialBaseline(context.Background(), nativeTestPrincipal(role), "key", nativeTestInput()); !errors.Is(err, ErrForbidden) {
			t.Fatalf("role=%s err=%v", role, err)
		}
	}
}

// TestNativeFinancialRejectsInvalidEvidenceAndPlans 验证本地依据不能靠假引用或计划差额入账。
// 参数：t 为测试上下文。
// 返回值：无；非法金额、日期、客户或分期被放行时失败。
func TestNativeFinancialRejectsInvalidEvidenceAndPlans(t *testing.T) {
	for _, change := range []func(*NativeFinancialInput){
		func(v *NativeFinancialInput) { v.CustomerKey = "7" }, func(v *NativeFinancialInput) { v.Amount = "1e99999999" },
		func(v *NativeFinancialInput) { v.PaymentTerms[1].Amount = "49.00" }, func(v *NativeFinancialInput) { v.PaymentTerms[1].InstallmentNo = 1 },
		func(v *NativeFinancialInput) { v.EvidenceReference = "" }, func(v *NativeFinancialInput) { v.EffectiveDate = "2020-02-30" },
	} {
		in := nativeTestInput()
		change(&in)
		if _, err := normalizeNativeFinancial(in, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)); err == nil {
			t.Fatal("非法本地依据通过校验")
		}
	}
}

// TestNativeCustomerUsesServerNamespace 验证客户记录只由本租户服务生成不透明引用。
// 参数：t 为测试上下文。
// 返回值：无；客户端身份或假CRM编号进入记录时失败。
func TestNativeCustomerUsesServerNamespace(t *testing.T) {
	var args []driver.NamedValue
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_financial_customer", Args: &args},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}, scriptdb.Step{Kind: "commit"})
	value, err := (&Service{DB: db, NativeFinancialEnabled: true}).CreateNativeFinancialCustomer(context.Background(), nativeTestPrincipal("settlement_finance_manager"), "key", NativeCustomerInput{Name: " 本地客户 "})
	if err != nil {
		t.Fatal(err)
	}
	if !nativeCustomerPattern.MatchString(value.CustomerKey) || value.Namespace != "settlement_local" || args[1].Value != "tenant-a" || args[2].Value != "本地客户" {
		t.Fatalf("customer=%+v args=%+v", value, args)
	}
}

// TestNativeBaselineCreatesOnlyLocalFacts 验证真实执行SQL只产生本地依据、计划与平台审计。
// 参数：t 为测试上下文。
// 返回值：无；创建合同stream/inbox/责任或绑定假合同ID时失败。
func TestNativeBaselineCreatesOnlyLocalFacts(t *testing.T) {
	var args []driver.NamedValue
	var statement string
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT name FROM settlement_financial_customer", Rows: [][]driver.Value{{"本地客户"}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_snapshot", Args: &args, SQLOut: &statement},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_receivable_plan"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_receivable_plan"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}, scriptdb.Step{Kind: "commit"})
	value, err := (&Service{DB: db, NativeFinancialEnabled: true}).CreateNativeFinancialBaseline(context.Background(), nativeTestPrincipal("settlement_finance_manager"), "key", nativeTestInput())
	if err != nil {
		t.Fatal(err)
	}
	if value.SourceContractID != nil || value.SourceContractNo != "" || value.BasisType != NativeFinancialBasis || value.CustomerKey != NativeCustomerPrefix+nativeTestCustomerID {
		t.Fatalf("伪造外部身份: %+v", value)
	}
	if len(args) != strings.Count(statement, "?") {
		t.Fatalf("占位符=%d 参数=%d", strings.Count(statement, "?"), len(args))
	}
	if args[1].Value != "tenant-a" || args[3].Value != value.CustomerKey || args[8].Value != nativeTestCustomerID {
		t.Fatalf("财务范围参数不正确: %+v", args)
	}
}

// TestNativeBaselineNullContractIDSurvivesQueryAndJSON 验证真实NULL不会导致字符串Scan或JSON伪标识。
// 参数：t 为测试上下文。
// 返回值：无；来源元数据丢失或产生假合同ID时失败。
func TestNativeBaselineNullContractIDSurvivesQueryAndJSON(t *testing.T) {
	row := []driver.Value{"snapshot-1", NativeFinancialBasis, "snapshot-1", "FB-1", int64(1), NativeCustomerPrefix + nativeTestCustomerID, "本地客户", "CNY", "100.00", "2020-01-01", "EFFECTIVE", "PURCHASE_ORDER", "PO-1", "原始依据", nil, "", "settlement_local"}
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "AND basis_type='LOCAL_FINANCIAL'", Rows: [][]driver.Value{row}})
	value, err := (&Service{DB: db, NativeFinancialEnabled: true}).GetNativeFinancialBaseline(context.Background(), nativeTestPrincipal("settlement_finance_manager"), "snapshot-1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil || !strings.Contains(string(data), `"source_contract_id":null`) || !strings.Contains(string(data), `"basis_type":"LOCAL_FINANCIAL"`) || !strings.Contains(string(data), `"customer_namespace":"settlement_local"`) {
		t.Fatalf("value=%s err=%v", data, err)
	}
}

// TestNativeVoidRetainsCancelledHistory 验证未消费本地依据只作废保留，不删除计划。
// 参数：t 为测试上下文。
// 返回值：无；未完整保留本地事实或漏版本保护时失败。
func TestNativeVoidRetainsCancelledHistory(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "query", SQL: "SELECT financial_status,local_document_version", Rows: [][]driver.Value{{"EFFECTIVE", int64(1)}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT confirmation_status", Rows: [][]driver.Value{{"PENDING_CONFIRMATION"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_receivable", Columns: 1}, scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_invoice_request", Columns: 1}, scriptdb.Step{Kind: "query", SQL: "SELECT dc.id", Columns: 1},
		scriptdb.Step{Kind: "exec", SQL: "SET financial_status='VOID'"}, scriptdb.Step{Kind: "exec", SQL: "SET confirmation_status='CANCELLED'"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}, scriptdb.Step{Kind: "commit"})
	err := (&Service{DB: db, NativeFinancialEnabled: true}).VoidNativeFinancialBaseline(context.Background(), nativeTestPrincipal("settlement_finance_manager"), "snapshot-1", "void-key", NativeFinancialVoidInput{Version: 1, Reason: "依据录入有误，作废重建"})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNativeVoidRejectsConfirmedAccounting 验证已确认计划不能借本地模式删除财务头。
// 参数：t 为测试上下文。
// 返回值：无；任何后续更新被执行时失败。
func TestNativeVoidRejectsConfirmedAccounting(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "query", SQL: "SELECT financial_status,local_document_version", Rows: [][]driver.Value{{"EFFECTIVE", int64(1)}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT confirmation_status", Rows: [][]driver.Value{{"CONFIRMED"}}}, scriptdb.Step{Kind: "rollback"})
	err := (&Service{DB: db, NativeFinancialEnabled: true}).VoidNativeFinancialBaseline(context.Background(), nativeTestPrincipal("settlement_finance_manager"), "snapshot-1", "void-key", NativeFinancialVoidInput{Version: 1, Reason: "禁止改头"})
	if !errors.Is(err, ErrNativeFinancialConsumed) {
		t.Fatal(err)
	}
}

// TestNativeBaselineDuplicateDocumentIsConflict 验证本地编号碰撞回滚为可识别业务冲突。
// 参数：t 为测试上下文。
// 返回值：无；SQL错误被当作不可分类内部错误或部分落库时失败。
func TestNativeBaselineDuplicateDocumentIsConflict(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT name FROM settlement_financial_customer", Rows: [][]driver.Value{{"客户"}}}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_snapshot", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}}, scriptdb.Step{Kind: "rollback"})
	_, err := (&Service{DB: db, NativeFinancialEnabled: true}).CreateNativeFinancialBaseline(context.Background(), nativeTestPrincipal("settlement_finance_manager"), "new-key", nativeTestInput())
	if !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

// TestNativePlanUsesExistingConfirmationAndBudget 验证空合同编号的本地计划沿原确认事务生成正式应收。
// 参数：t 为测试上下文。
// 返回值：无；绕过覆盖守卫、创建外部身份或未提交账务时失败。
func TestNativePlanUsesExistingConfirmationAndBudget(t *testing.T) {
	p := nativeTestPrincipal("settlement_accountant")
	p.Permissions["settlement.receivable.confirm"] = true
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT rp.contract_snapshot_id", Rows: [][]driver.Value{{"local-snapshot", "2020-02-01", "50.00", "CNY"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state", Rows: [][]driver.Value{{"RELEASED"}}}, scriptdb.Step{Kind: "query", SQL: "SELECT migration_state", Rows: [][]driver.Value{{"RELEASED"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT contract_amount,source_contract_no", Rows: [][]driver.Value{{"100.00", ""}}}, scriptdb.Step{Kind: "query", SQL: "SELECT COALESCE(SUM(CASE WHEN id<>", Rows: [][]driver.Value{{"50.00", int64(0)}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable_plan SET due_date"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_receivable("}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}, scriptdb.Step{Kind: "commit"})
	if err := (&Service{DB: db, NativeFinancialEnabled: true}).ConfirmPlan(context.Background(), p, "local-plan", PlanConfirmationInput{Version: 1}); err != nil {
		t.Fatal(err)
	}
}
