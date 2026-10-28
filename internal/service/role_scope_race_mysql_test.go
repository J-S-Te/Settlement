package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

type scopeRaceUpdate struct {
	rows int64
	err  error
}

// roleScopeRaceFixture 在随机独占schema中建立销售和助理的真实合同责任。
// 参数：t 为测试上下文，role 为当前需要验证的受限角色。
// 返回值：服务、可信主体和合同快照标识。
func roleScopeRaceFixture(t *testing.T, role string) (*Service, Principal, string) {
	t.Helper()
	db, _ := invoiceArchiveMySQL(t)
	snapshot := newID()
	steps := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,'tenant','contract_management','event','contract','HT-1',1,'customer','customer','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{snapshot}},
		{`INSERT INTO settlement_contract_responsibility(tenant_id,source_contract_id,owner_user_id,version,active,updated_at) VALUES('tenant','contract','actor',1,TRUE,UTC_TIMESTAMP(3))`, nil},
		{`INSERT INTO settlement_project_reference(tenant_id,project_id,source_contract_id,customer_id,project_name,version,active,updated_at) VALUES('tenant','project','contract','customer','project',1,TRUE,UTC_TIMESTAMP(3))`, nil},
		{`INSERT INTO settlement_project_assignment(tenant_id,project_id,user_id,version,active,updated_at,updated_by) VALUES('tenant','project','actor',1,TRUE,UTC_TIMESTAMP(3),'manager')`, nil},
	}
	for _, step := range steps {
		if _, err := db.Exec(step.query, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	p := Principal{TenantID: "tenant", UserID: "actor", CatalogVersion: "scope-race-test", Roles: []string{role}, Permissions: map[string]bool{"settlement.receivable.read": true}}
	return &Service{DB: db}, p, snapshot
}

// waitRoleScopeRevokeBlocked 通过MySQL锁等待记录确认撤销语句确实被业务事务阻塞。
// 参数：t 为测试上下文，ctx 控制查询，db 为独占schema连接池，connectionID 为撤销连接，done 为执行结果。
// 返回值：无；提前完成或未形成真实锁等待时使测试失败。
func waitRoleScopeRevokeBlocked(t *testing.T, ctx context.Context, db *sql.DB, connectionID uint64, done <-chan scopeRaceUpdate) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		var waiting int
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.data_lock_waits w JOIN performance_schema.threads th ON th.THREAD_ID=w.REQUESTING_THREAD_ID WHERE th.PROCESSLIST_ID=?`, connectionID).Scan(&waiting)
		if err != nil {
			t.Fatalf("无法核验真实MySQL锁等待：%v", err)
		}
		if waiting > 0 {
			return
		}
		select {
		case result := <-done:
			t.Fatalf("撤销未等待业务事务，rows=%d err=%v", result.rows, result.err)
		case <-deadline.C:
			t.Fatal("撤销连接未进入预期MySQL锁等待")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

// TestMySQLContractScopeRevocationWaitsForBusinessTx 验证销售及助理的实际责任行保持到业务事务结束。
// 参数：t 为测试上下文；仅显式隔离SETTLEMENT_TEST_DSN启用真实MySQL。
// 返回值：无。
func TestMySQLContractScopeRevocationWaitsForBusinessTx(t *testing.T) {
	cases := []struct{ name, role, revokeSQL string }{
		{"sales_owner", "settlement_sales", `UPDATE settlement_contract_responsibility SET owner_user_id='successor',version=version+1 WHERE tenant_id='tenant' AND source_contract_id='contract'`},
		{"assistant_assignment", "settlement_project_assistant", `UPDATE settlement_project_assignment SET active=FALSE,version=version+1 WHERE tenant_id='tenant' AND project_id='project' AND user_id='actor'`},
		{"assistant_project_reference", "settlement_project_assistant", `UPDATE settlement_project_reference SET active=FALSE,version=version+1 WHERE tenant_id='tenant' AND project_id='project'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, p, snapshot := roleScopeRaceFixture(t, tc.role)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			txA, err := s.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer txA.Rollback()
			if err = s.RequireContractAccessTx(ctx, txA, p, "settlement.receivable.read", snapshot); err != nil {
				t.Fatalf("事务A初始授权失败：%v", err)
			}
			connB, err := s.DB.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); _ = txA.Rollback(); _ = connB.Close() }()
			var idB uint64
			if err = connB.QueryRowContext(ctx, `SELECT CONNECTION_ID()`).Scan(&idB); err != nil {
				t.Fatal(err)
			}
			txB, err := connB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer txB.Rollback()
			done := make(chan scopeRaceUpdate, 1)
			go func() {
				result, err := txB.ExecContext(ctx, tc.revokeSQL)
				if err != nil {
					done <- scopeRaceUpdate{err: err}
					return
				}
				rows, err := result.RowsAffected()
				if err == nil {
					err = txB.Commit()
				}
				done <- scopeRaceUpdate{rows: rows, err: err}
			}()
			waitRoleScopeRevokeBlocked(t, ctx, s.DB, idB, done)
			if err = txA.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-done:
				if result.err != nil || result.rows != 1 {
					t.Fatalf("业务提交后撤销未成功：%+v", result)
				}
			case <-ctx.Done():
				t.Fatal("业务提交后撤销仍未完成")
			}
			if err = s.RequireContractAccess(ctx, p, "settlement.receivable.read", snapshot); !errors.Is(err, ErrNotFound) {
				t.Fatalf("撤销提交后旧主体仍可访问：%v", err)
			}
		})
	}
}

// TestMySQLScopeRevocationDoesNotReuseOldReadView 验证事务先有普通读快照时也不能沿用已撤销责任。
// 参数：t 为测试上下文；仅显式隔离SETTLEMENT_TEST_DSN启用真实MySQL。
// 返回值：无。
func TestMySQLScopeRevocationDoesNotReuseOldReadView(t *testing.T) {
	for _, role := range []string{"settlement_sales", "settlement_project_assistant"} {
		t.Run(role, func(t *testing.T) {
			s, p, snapshot := roleScopeRaceFixture(t, role)
			ctx := context.Background()
			tx, err := s.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_contract_responsibility WHERE tenant_id='tenant'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			query := `UPDATE settlement_contract_responsibility SET owner_user_id='successor',version=version+1 WHERE tenant_id='tenant' AND source_contract_id='contract'`
			if role == "settlement_project_assistant" {
				query = `UPDATE settlement_project_assignment SET active=FALSE,version=version+1 WHERE tenant_id='tenant' AND project_id='project' AND user_id='actor'`
			}
			if _, err = s.DB.ExecContext(ctx, query); err != nil {
				t.Fatal(err)
			}
			if err = s.RequireContractAccessTx(ctx, tx, p, "settlement.receivable.read", snapshot); !errors.Is(err, ErrNotFound) {
				t.Fatalf("旧读视图绕过已提交撤销：%v", err)
			}
		})
	}
}
