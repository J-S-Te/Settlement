package service

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/settlement/internal/projectreference"
)

type assignmentReference struct{ reference projectreference.Reference }

// Resolve 用途：提供隔离组件测试的权威接口结果，HTTP契约另有专门测试。
// 参数：上下文、租户和项目用于匹配测试调用。
// 返回值：返回受控引用，不代表真实项目服务已联调。
func (f assignmentReference) Resolve(context.Context, string, string) (projectreference.Reference, error) {
	return f.reference, nil
}

type assignmentDirectory struct{}

// ContainsRole 用途：提供隔离协作事务测试中的有效人员资格。
// 参数：上下文、用户和角色为待核验履职对象。
// 返回值：测试中返回有效，真实HTTP目录另有契约测试。
func (assignmentDirectory) ContainsRole(context.Context, string, string) (bool, error) {
	return true, nil
}

// TestMySQLProjectAssignmentScopeLifecycle 用途：验证项目协作保存、撤销和跨租户引用拒绝。
// 参数：t为测试上下文，仅显式隔离DSN的新建schema运行。
// 返回值：无，越权、错误映射或撤销未失效时失败。
func TestMySQLProjectAssignmentScopeLifecycle(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	tenant := "assignment-tenant"
	snapshot := newID()
	_, err := db.Exec(`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,'contract_management',?,'c-a','C-A',1,'cust-a','隔离客户','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, snapshot, tenant, newID())
	if err != nil {
		t.Fatal(err)
	}
	ref := projectreference.Reference{TenantID: tenant, ProjectID: "project-a", ContractID: "c-a", CustomerID: "cust-a", Name: "隔离项目", Version: 1, Active: true}
	svc := &Service{DB: db, ProjectReferences: assignmentReference{ref}, RoleDirectory: assignmentDirectory{}}
	manager := roleFlowPrincipal("settlement_finance_manager", "manager", tenant)
	assistant := roleFlowPrincipal("settlement_project_assistant", "assistant", tenant)
	if err = svc.RequireContractAccess(ctx, assistant, "settlement.invoice.request", snapshot); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	in := ProjectAssignmentInput{ProjectID: ref.ProjectID, UserID: assistant.UserID, Active: true}
	if err = svc.AssignProject(ctx, manager, in); err != nil {
		t.Fatal(err)
	}
	if err = svc.RequireContractAccess(ctx, assistant, "settlement.invoice.request", snapshot); err != nil {
		t.Fatal(err)
	}
	if err = svc.UpdateProjectMappingNote(ctx, assistant, ref.ProjectID, 1, "已核对项目映射"); err != nil {
		t.Fatal(err)
	}
	in.Version = 2
	in.Active = false
	svc.ProjectReferences = nil
	svc.RoleDirectory = nil
	if err = svc.AssignProject(ctx, manager, in); err != nil {
		t.Fatal(err)
	}
	if err = svc.RequireContractAccess(ctx, assistant, "settlement.invoice.request", snapshot); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	ref.TenantID = "other-tenant"
	svc.ProjectReferences = assignmentReference{ref}
	svc.RoleDirectory = assignmentDirectory{}
	in.Active = true
	in.Version = 3
	if err = svc.AssignProject(ctx, manager, in); !errors.Is(err, ErrInvalid) {
		t.Fatalf("跨租户引用: %v", err)
	}
}
