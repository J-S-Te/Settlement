package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestListContractRevisionHoldsPageFilters 验证工单分页按状态/合同筛选取并绑定
// 租户、范围与分页参数，供财务按合同定位差异。
// 参数：t 为测试上下文。
// 返回值：无；筛选未下推、参数顺序不符或响应字段缺失时失败。
func TestListContractRevisionHoldsPageFilters(t *testing.T) {
	var args []driver.NamedValue
	var sqlText string
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_revision_hold cs", Args: &args, SQLOut: &sqlText, Rows: [][]driver.Value{
		{"hold-2", "contract-2", "HT-2", "snapshot-2", "EVT-5", int64(1), int64(3), contractRevisionHoldTermTotal, []byte(`{"diffs":[]}`), contractRevisionHoldStatusResolved, "已按修订金额完成调整", "finance-manager", "2026-10-31T08:00:00.000000Z"},
	}})
	svc := &Service{DB: db}
	p := Principal{TenantID: "tenant-a", UserID: "finance", Permissions: map[string]bool{"settlement.revision.read": true}}
	items, err := svc.ListContractRevisionHoldsPage(context.Background(), p, ContractRevisionHoldQuery{Limit: 25, Offset: 10, Status: contractRevisionHoldStatusResolved, ContractRef: "HT-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "hold-2" || items[0].Status != contractRevisionHoldStatusResolved || items[0].ResolvedBy != "finance-manager" {
		t.Fatalf("工单分页=%+v", items)
	}
	if !strings.Contains(sqlText, "cs.status=?") || !strings.Contains(sqlText, "cs.source_contract_id=? OR cs.source_contract_no=?") {
		t.Fatalf("筛选未下推到SQL: %s", sqlText)
	}
	want := []any{"tenant-a", contractRevisionHoldStatusResolved, "HT-2", "HT-2", int64(25), int64(10)}
	if len(args) != len(want) {
		t.Fatalf("绑定参数=%+v", args)
	}
	for i, expected := range want {
		if args[i].Value != expected {
			t.Fatalf("参数%d=%v，期望 %v", i, args[i].Value, expected)
		}
	}
}

// TestListContractRevisionHoldsPageRejectsInvalidQuery 验证越界分页、未知状态与
// 过长合同筛选在访问数据库前被拒绝。
// 参数：t 为测试上下文。
// 返回值：无；非法查询进入数据库时失败。
func TestListContractRevisionHoldsPageRejectsInvalidQuery(t *testing.T) {
	svc := &Service{}
	p := Principal{TenantID: "tenant-a", UserID: "finance", Permissions: map[string]bool{"settlement.revision.read": true}}
	for _, query := range []ContractRevisionHoldQuery{
		{Limit: -1},
		{Limit: 201},
		{Offset: -1},
		{Offset: 100001},
		{Status: "UNKNOWN"},
		{ContractRef: strings.Repeat("a", 129)},
	} {
		if _, err := svc.ListContractRevisionHoldsPage(context.Background(), p, query); !errors.Is(err, ErrInvalid) {
			t.Fatalf("查询 %+v 必须拒绝: %v", query, err)
		}
	}
	unprivileged := Principal{TenantID: "tenant-a", UserID: "sales"}
	if _, err := svc.ListContractRevisionHoldsPage(context.Background(), unprivileged, ContractRevisionHoldQuery{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("缺少只读权限必须禁止: %v", err)
	}
	// 旧的应收只读权限不再能查看修订工单：列表只认专用只读能力。
	legacyRead := Principal{TenantID: "tenant-a", UserID: "finance", Permissions: map[string]bool{"settlement.receivable.read": true}}
	if _, err := svc.ListContractRevisionHoldsPage(context.Background(), legacyRead, ContractRevisionHoldQuery{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("仅 settlement.receivable.read 不得查看修订工单: %v", err)
	}
}

// TestListContractRevisionHoldsKeepsResponsibilityScope 验证改用专用只读权限后，
// 销售仍按合同责任范围过滤，不因权限收敛而放宽可见数据。
// 参数：t 为测试上下文。
// 返回值：无；责任谓词缺失或租户/用户绑定不符时失败。
func TestListContractRevisionHoldsKeepsResponsibilityScope(t *testing.T) {
	var args []driver.NamedValue
	var sqlText string
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_contract_revision_hold cs", Args: &args, SQLOut: &sqlText})
	svc := &Service{DB: db}
	p := Principal{
		TenantID: "tenant-a", UserID: "sales-1", Roles: []string{"settlement_sales"},
		DataScopes: []DataScope{{RoleCode: "settlement_sales", ScopeType: "APPLICATION"}}, CatalogVersion: "4",
		Permissions: map[string]bool{"settlement.revision.read": true},
	}
	if _, err := svc.ListContractRevisionHoldsPage(context.Background(), p, ContractRevisionHoldQuery{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sqlText, "settlement_contract_responsibility") {
		t.Fatalf("销售必须按合同责任范围过滤: %s", sqlText)
	}
	if len(args) < 2 || args[0].Value != "tenant-a" || args[1].Value != "sales-1" {
		t.Fatalf("租户/责任人绑定=%+v", args)
	}
}
