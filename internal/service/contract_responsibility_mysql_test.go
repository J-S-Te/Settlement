package service

import (
	"context"
	"errors"
	"testing"
)

// TestMySQLResponsibilityChangesImmediatelyRevokeOldSales 验证真实责任交接、撤销与迟到事件的数据范围。
// 参数：t 为测试上下文；仅显式隔离SETTLEMENT_TEST_DSN启用，复用随机独占schema。
// 返回值：无。
func TestMySQLResponsibilityChangesImmediatelyRevokeOldSales(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	s := &Service{DB: db}
	snapshot := newID()
	_, err := db.ExecContext(ctx, `INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,'tenant','contract_management','financial','C1','HT-1',1,'42','customer','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	sales := func(owner string) Principal {
		return Principal{TenantID: "tenant", UserID: owner, CatalogVersion: "test-role-catalog", Roles: []string{"settlement_sales"}, Permissions: map[string]bool{"settlement.receivable.read": true}}
	}
	event := responsibilityEvent()
	event.SourceVersion = 1
	event.OwnerUserID = "owner-old"
	if applied, err := s.IngestContractResponsibility(ctx, "tenant", event); err != nil || !applied {
		t.Fatalf("初始责任=%v %v", applied, err)
	}
	if err = s.RequireContractAccess(ctx, sales("owner-old"), "settlement.receivable.read", snapshot); err != nil {
		t.Fatalf("初始负责人不可读：%v", err)
	}
	original := event
	event.EventID = "R2"
	event.SourceVersion = 2
	event.OwnerUserID = "owner-new"
	if applied, err := s.IngestContractResponsibility(ctx, "tenant", event); err != nil || !applied {
		t.Fatalf("交接=%v %v", applied, err)
	}
	if err = s.RequireContractAccess(ctx, sales("owner-old"), "settlement.receivable.read", snapshot); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧负责人仍可读：%v", err)
	}
	if err = s.RequireContractAccess(ctx, sales("owner-new"), "settlement.receivable.read", snapshot); err != nil {
		t.Fatalf("新负责人不可读：%v", err)
	}
	if applied, err := s.IngestContractResponsibility(ctx, "tenant", event); err != nil || applied {
		t.Fatalf("重复事件非幂等：%v %v", applied, err)
	}
	original.EventID = "late-initial"
	if applied, err := s.IngestContractResponsibility(ctx, "tenant", original); err != nil || applied {
		t.Fatalf("迟到事件覆盖责任：%v %v", applied, err)
	}
	event.EventID = "R3"
	event.SourceVersion = 3
	event.Active = false
	if applied, err := s.IngestContractResponsibility(ctx, "tenant", event); err != nil || !applied {
		t.Fatalf("撤销=%v %v", applied, err)
	}
	if err = s.RequireContractAccess(ctx, sales("owner-new"), "settlement.receivable.read", snapshot); !errors.Is(err, ErrNotFound) {
		t.Fatalf("撤销后仍可读：%v", err)
	}
	event.OwnerUserID = "injected"
	if _, err = s.IngestContractResponsibility(ctx, "tenant", event); !errors.Is(err, ErrConflict) {
		t.Fatalf("事件ID内容冲突未拒绝：%v", err)
	}
	var amount string
	if err = db.QueryRowContext(ctx, `SELECT contract_amount FROM settlement_contract_snapshot WHERE id=?`, snapshot).Scan(&amount); err != nil || amount != "100.00" {
		t.Fatalf("责任流改写财务金额：%s %v", amount, err)
	}
}
