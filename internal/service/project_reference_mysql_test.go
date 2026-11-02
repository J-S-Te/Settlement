package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/j-s-te/settlement/internal/projectreference"
)

// readProjectReference 读取隔离库中的项目引用快照，用于断言权威状态落地结果。
// 参数：t 测试上下文，db 隔离库，tenant 租户，project 项目。
// 返回值：快照状态；查询失败使测试失败。
func readProjectReference(t *testing.T, db *sql.DB, tenant, project string) projectReferenceState {
	t.Helper()
	var state projectReferenceState
	if err := db.QueryRow(`SELECT version,source_contract_id,customer_id,project_name,active FROM settlement_project_reference WHERE tenant_id=? AND project_id=?`, tenant, project).Scan(&state.Version, &state.ContractID, &state.CustomerID, &state.Name, &state.Active); err != nil {
		t.Fatalf("read project reference: %v", err)
	}
	return state
}

// saveAuthorityReference 在独立事务中按权威引用同步本地快照，模拟一次项目服务拉取。
// 参数：t 测试上下文，svc 服务，tenant 租户，ref 权威引用。
// 返回值：服务错误，供断言冲突语义。
func saveAuthorityReference(t *testing.T, svc *Service, tenant string, ref projectreference.Reference) error {
	t.Helper()
	ctx := context.Background()
	tx, err := svc.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = svc.saveProjectReference(ctx, tx, tenant, ref); err != nil {
		return err
	}
	return tx.Commit()
}

// TestMySQLProjectReferenceSnapshotFollowsAuthority 验证真实迁移下项目引用快照：
// active 随权威端下降而不是只升不降，同版本换合同返回可识别的澄清冲突且不覆盖，
// 版本升高后的合同更换受控生效，迟到版本被拒绝。
// 参数：t 测试上下文；仅显式隔离 SETTLEMENT_TEST_DSN 运行。
// 返回值：无。
func TestMySQLProjectReferenceSnapshotFollowsAuthority(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	tenant, project := "project-reference-authority", "project-a"
	svc := &Service{DB: db}
	base := projectreference.Reference{TenantID: tenant, ProjectID: project, ContractID: "contract-a", CustomerID: "customer-a", Name: "项目", Version: 1, Active: true}
	if err := saveAuthorityReference(t, svc, tenant, base); err != nil {
		t.Fatal(err)
	}
	if got := readProjectReference(t, db, tenant, project); got != (projectReferenceState{Version: 1, ContractID: "contract-a", CustomerID: "customer-a", Name: "项目", Active: true}) {
		t.Fatalf("initial snapshot=%+v", got)
	}
	// 同一版本、同一合同：权威端关闭项目时 active 必须下降，不能保持只升不降。
	deactivated := base
	deactivated.Active = false
	if err := saveAuthorityReference(t, svc, tenant, deactivated); err != nil {
		t.Fatal(err)
	}
	if got := readProjectReference(t, db, tenant, project); got.Active || got.Version != 1 || got.ContractID != "contract-a" {
		t.Fatalf("same-version deactivation did not land: %+v", got)
	}
	// 同一版本更换合同：返回澄清冲突，已对账快照保持原合同不被覆盖。
	contradiction := base
	contradiction.ContractID = "contract-b"
	if err := saveAuthorityReference(t, svc, tenant, contradiction); !errors.Is(err, ErrProjectReferenceMismatch) {
		t.Fatalf("same-version contract change err=%v want mismatch", err)
	}
	if got := readProjectReference(t, db, tenant, project); got.ContractID != "contract-a" || got.Active || got.Version != 1 {
		t.Fatalf("mismatched authority overwrote reconciled snapshot: %+v", got)
	}
	// 权威端提升版本：合同更换是受控更新，并重新激活项目映射。
	upgraded := base
	upgraded.Version, upgraded.ContractID, upgraded.Name = 2, "contract-b", "新项目"
	if err := saveAuthorityReference(t, svc, tenant, upgraded); err != nil {
		t.Fatal(err)
	}
	if got := readProjectReference(t, db, tenant, project); got != (projectReferenceState{Version: 2, ContractID: "contract-b", CustomerID: "customer-a", Name: "新项目", Active: true}) {
		t.Fatalf("versioned update did not land: %+v", got)
	}
	// 迟到版本不得回退本地快照。
	if err := saveAuthorityReference(t, svc, tenant, base); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale authority err=%v want conflict", err)
	}
	if got := readProjectReference(t, db, tenant, project); got.Version != 2 || got.ContractID != "contract-b" {
		t.Fatalf("stale authority rolled snapshot back: %+v", got)
	}
}

// TestMySQLProjectAssignmentInactiveAuthorityStopsScope 验证权威项目失效时：
// 分配请求被拒绝但本地快照按权威版本收口，已分配助理不再获得合同范围。
// 参数：t 测试上下文；仅显式隔离 SETTLEMENT_TEST_DSN 运行。
// 返回值：无。
func TestMySQLProjectAssignmentInactiveAuthorityStopsScope(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	tenant, snapshot := "project-reference-inactive", newID()
	if _, err := db.Exec(`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,'contract_management',?,'contract-a','C-A',1,'customer-a','隔离客户','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, snapshot, tenant, newID()); err != nil {
		t.Fatal(err)
	}
	ref := projectreference.Reference{TenantID: tenant, ProjectID: "project-a", ContractID: "contract-a", CustomerID: "customer-a", Name: "项目", Version: 1, Active: true}
	svc := &Service{DB: db, ProjectReferences: assignmentReference{reference: ref}, RoleDirectory: assignmentDirectory{}}
	manager := roleFlowPrincipal("settlement_finance_manager", "manager", tenant)
	assistant := roleFlowPrincipal("settlement_project_assistant", "assistant", tenant)
	if err := svc.AssignProject(ctx, manager, ProjectAssignmentInput{ProjectID: "project-a", UserID: assistant.UserID, Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RequireContractAccess(ctx, assistant, "settlement.receivable.read", snapshot); err != nil {
		t.Fatalf("active project should grant scope: %v", err)
	}
	// 权威端以新版本关闭项目：分配必须被拒绝，快照 active 必须下降。
	inactive := ref
	inactive.Version, inactive.Active = 2, false
	svc.ProjectReferences = assignmentReference{reference: inactive}
	if err := svc.AssignProject(ctx, manager, ProjectAssignmentInput{ProjectID: "project-a", UserID: assistant.UserID, Active: true}); !errors.Is(err, ErrProjectReferenceInactive) {
		t.Fatalf("inactive authority err=%v want inactive", err)
	}
	if got := readProjectReference(t, db, tenant, "project-a"); got.Version != 2 || got.Active {
		t.Fatalf("inactive authority not persisted: %+v", got)
	}
	if err := svc.RequireContractAccess(ctx, assistant, "settlement.receivable.read", snapshot); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inactive project still grants scope: %v", err)
	}
}
