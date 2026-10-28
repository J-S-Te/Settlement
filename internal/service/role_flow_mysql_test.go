package service

import (
	"context"
	"errors"
	"testing"
)

// roleFlowPrincipal 用途：构造与当前目录一致的隔离测试角色主体。
// 参数：role为目录角色，user为测试用户，tenant为测试租户。
// 返回值：返回带真实角色权限及APPLICATION范围的测试主体。
func roleFlowPrincipal(role, user, tenant string) Principal {
	return Principal{TenantID: tenant, UserID: user, IdentityID: user, Roles: []string{role}, Permissions: catalogRoleGrants[role], CatalogVersion: "3", DataScopes: []DataScope{{RoleCode: role, ScopeType: "APPLICATION"}}}
}

// TestMySQLRolePlanAuthorizationAndBudget 用途：以真实迁移验证本人确认、跨人拒绝和跨期预算。
// 参数：t为测试上下文；仅显式SETTLEMENT_TEST_DSN的独占schema运行。
// 返回值：无，任何越权、超额或确认金额错误时失败。
func TestMySQLRolePlanAuthorizationAndBudget(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	tenant := "role-flow"
	snapshot, plan := newID(), newID()
	_, err := db.Exec(`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,'contract_management',?,'contract-a','C-A',1,'customer-a','隔离客户','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, snapshot, tenant, newID())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,created_at,updated_at) VALUES(?,?,?,1,'2099-12-01',100,'CONTRACT_EVENT','PENDING_CONFIRMATION',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, plan, tenant, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO settlement_contract_responsibility(tenant_id,source_contract_id,owner_user_id,version,active,updated_at) VALUES(?,'contract-a','sales-a',1,TRUE,UTC_TIMESTAMP(3))`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: db}
	owner := roleFlowPrincipal("settlement_sales", "sales-a", tenant)
	other := roleFlowPrincipal("settlement_sales", "sales-b", tenant)
	if err = svc.ConfirmPlan(ctx, other, plan, PlanConfirmationInput{Version: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("同租户其他销售确认: %v", err)
	}
	if err = svc.ConfirmPlan(ctx, owner, plan, PlanConfirmationInput{Version: 1, PlannedAmount: "101.00", Reason: "非法超额"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("超额确认: %v", err)
	}
	if err = svc.ConfirmPlan(ctx, owner, plan, PlanConfirmationInput{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err = svc.AdjustPlan(ctx, owner, plan, PlanConfirmationInput{Version: 2, DueDate: "2099-12-02", PlannedAmount: "80.00", Reason: "分期调整"}); err != nil {
		t.Fatal(err)
	}
	id, err := svc.CreateManualPlan(ctx, owner, "new-part", ManualPlanInput{ContractSnapshotID: snapshot, InstallmentNo: 2, DueDate: "2099-12-03", PlannedAmount: "20.00", Reason: "新增分期"})
	if err != nil {
		t.Fatal(err)
	}
	if replay, e := svc.CreateManualPlan(ctx, owner, "new-part", ManualPlanInput{ContractSnapshotID: snapshot, InstallmentNo: 2, DueDate: "2099-12-03", PlannedAmount: "20.00", Reason: "新增分期"}); e != nil || replay != id {
		t.Fatalf("计划幂等: %s %v", replay, e)
	}
	if _, err = svc.CreateManualPlan(ctx, owner, "too-much", ManualPlanInput{ContractSnapshotID: snapshot, InstallmentNo: 3, DueDate: "2099-12-03", PlannedAmount: "1.00", Reason: "超额"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("跨期超额: %v", err)
	}
}
