package dunning

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/migration"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/migrations"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// roleReminderMySQL 创建并清理独占隔离测试schema，执行真实迁移。
// 参数：t 测试上下文。
// 返回值：隔离数据库及其连接配置。
func roleReminderMySQL(t *testing.T) (*sql.DB, *mysql.Config) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_TEST_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_TEST_DSN not configured; real MySQL invoice archive acceptance not executed")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !regexp.MustCompile(`^settlement_e2e(_[a-z0-9_]+)?$`).MatchString(cfg.DBName) {
		t.Fatal("SETTLEMENT_TEST_DSN must identify settlement_e2e[_namespace]; DSN withheld")
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "settlement_e2e_reminder_" + hex.EncodeToString(nonce[:])
	controlCfg := cfg.Clone()
	controlCfg.DBName = ""
	control, err := sql.Open("mysql", controlCfg.FormatDSN())
	if err != nil {
		t.Fatal("open test control connection failed")
	}
	t.Cleanup(func() { control.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err = control.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create owned invoice archive database failed; DSN withheld")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := control.ExecContext(ctx, "DROP DATABASE `"+name+"`"); err != nil {
			t.Error("drop owned invoice archive database failed; DSN withheld")
		}
	})
	runCfg := cfg.Clone()
	runCfg.DBName = name
	runCfg.ParseTime = true
	runCfg.MultiStatements = true
	if err = migration.Run(ctx, runCfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatalf("apply real Settlement migrations: %v", err)
	}
	db, err := sql.Open("mysql", runCfg.FormatDSN())
	if err != nil {
		t.Fatal("open owned archive database failed")
	}
	t.Cleanup(func() { db.Close() })
	return db, runCfg
}

type roleDirectoryFixture struct{ failure bool }

// ContainsRole 返回测试角色资格或指定依赖故障。
// 参数：ctx 上下文，user 用户，role 角色。
// 返回值：资格及模拟目录故障。
func (d *roleDirectoryFixture) ContainsRole(ctx context.Context, user, role string) (bool, error) {
	if d.failure {
		return false, errors.New("directory unavailable")
	}
	return user == "sales", nil
}

// ListRole 返回独立测试的有效升级接收人。
// 参数：ctx 上下文，role 角色，keyword 搜索词，page 页码，size 容量。
// 返回值：实际测试接收人及总数或故障。
func (d *roleDirectoryFixture) ListRole(ctx context.Context, role, keyword string, page, size int) ([]platform.Personnel, int64, error) {
	if d.failure {
		return nil, 0, errors.New("directory unavailable")
	}
	return []platform.Personnel{{UserID: role}}, 1, nil
}

// TestMySQLRoleReminderLifecycle 验证真实迁移、并发幂等、个人反馈、升级及结清停止。
// 参数：t 测试上下文。
// 返回值：无。
func TestMySQLRoleReminderLifecycle(t *testing.T) {
	db, _ := roleReminderMySQL(t)
	ctx := t.Context()
	tenant, snapshot, plan, receivable := "tenant-role-reminder", id(), id(), id()
	statements := []struct {
		q string
		a []any
	}{
		{`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,'test','event','contract','C001',1,'customer','测试客户','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{snapshot, tenant}},
		{`INSERT INTO settlement_contract_responsibility(tenant_id,source_contract_id,owner_user_id,version,active,updated_at) VALUES(?,'contract','sales',1,TRUE,UTC_TIMESTAMP(3))`, []any{tenant}},
		{`INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,created_at,updated_at) VALUES(?,?,?,1,DATE_ADD(CURDATE(),INTERVAL 3 DAY),100,'CONTRACT','PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{plan, tenant, snapshot}},
	}
	for _, step := range statements {
		if _, err := db.ExecContext(ctx, step.q, step.a...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO settlement_dunning_policy(id,tenant_id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled,created_at,updated_at) VALUES(?,?,'既有催收员策略',1,1,36500,'REMIND','USER:collector','LOCAL',7,'NORMAL',TRUE,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id(), tenant); err != nil {
		t.Fatal(err)
	}
	directory := &roleDirectoryFixture{}
	svc := &service.Service{DB: db}
	worker := &RoleReminderWorker{DB: db, Service: svc, TenantID: tenant, Directory: directory}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- worker.RunOnce(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var actions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=?`, tenant).Scan(&actions); err != nil || actions != 1 {
		t.Fatalf("concurrent initial actions=%d err=%v", actions, err)
	}
	var action string
	if err := db.QueryRow(`SELECT id FROM settlement_dunning_action WHERE tenant_id=?`, tenant).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if err := svc.CompleteDunningAction(ctx, service.Principal{TenantID: tenant, UserID: "sales", Roles: []string{"settlement_sales"}, CatalogVersion: "test", Permissions: map[string]bool{"settlement.dunning.feedback": true}}, action, 1, "已联系客户确认计划"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE settlement_receivable_plan SET due_date=DATE_SUB(CURDATE(),INTERVAL 1 DAY),confirmation_status='CONFIRMED' WHERE id=?`, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,receivable_plan_id,due_date,original_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,?,'R001',?,?,DATE_SUB(CURDATE(),INTERVAL 1 DAY),100,100,'CNY','CONFIRMED','UNPAID','UNINVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, receivable, tenant, snapshot, plan); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=?`, tenant).Scan(&actions); err != nil || actions != 2 {
		t.Fatalf("overdue actions=%d err=%v", actions, err)
	}
	if _, err := db.Exec(`UPDATE settlement_role_reminder_cursor SET next_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 SECOND) WHERE tenant_id=? AND phase='OVERDUE'`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settlement_handover_freeze(id,tenant_id,batch_id,request_id,user_id,status,frozen_at,created_at,updated_at) VALUES(?,?,?,?,'sales','FROZEN',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id(), tenant, id(), id()); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=?`, tenant).Scan(&actions); err != nil || actions != 2 {
		t.Fatalf("frozen actions=%d err=%v", actions, err)
	}
	if _, err := db.Exec(`UPDATE settlement_handover_freeze SET status='RELEASED' WHERE tenant_id=?`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=?`, tenant).Scan(&actions); err != nil || actions != 3 {
		t.Fatalf("repeat actions=%d err=%v", actions, err)
	}
	if _, err := db.Exec(`UPDATE settlement_receivable_plan SET due_date=DATE_SUB(CURDATE(),INTERVAL 30 DAY) WHERE id=?`, plan); err != nil {
		t.Fatal(err)
	}
	directory.failure = true
	if err := worker.RunOnce(ctx); err == nil {
		t.Fatal("directory outage accepted")
	}
	directory.failure = false
	if _, err := db.Exec(`UPDATE settlement_role_reminder_cursor SET next_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 SECOND) WHERE tenant_id=? AND phase='ESCALATED'`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=?`, tenant).Scan(&actions); err != nil || actions != 4 {
		t.Fatalf("escalated actions=%d err=%v", actions, err)
	}
	var escalatedNotifications int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_local_notification WHERE tenant_id=? AND priority='HIGH'`, tenant).Scan(&escalatedNotifications); err != nil || escalatedNotifications != 3 {
		t.Fatalf("escalated notifications=%d err=%v", escalatedNotifications, err)
	}
	if _, err := db.Exec(`UPDATE settlement_receivable SET open_amount=0 WHERE id=?`, receivable); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=? AND status='PENDING'`, tenant).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("settled pending=%d err=%v", pending, err)
	}
}

type starvationDirectory struct {
	roleDirectoryFixture
	recovered bool
	outage    bool
}

// ContainsRole 为队首无效主体提供可恢复的目录故障或角色撤销场景。
// 参数：ctx 上下文，user 用户标识，role 角色。
// 返回值：当前履职资格及目录故障。
func (d *starvationDirectory) ContainsRole(ctx context.Context, user, role string) (bool, error) {
	if user == "sales" {
		return true, nil
	}
	if d.recovered {
		return true, nil
	}
	if d.outage {
		return false, errors.New("owner directory unavailable")
	}
	return false, nil
}

// TestMySQLRoleReminderDoesNotStarveBehindInvalidOwners 验证队首失败候选不会永久挡住后续合法负责人。
// 参数：t 测试上下文。
// 返回值：无。
func TestMySQLRoleReminderDoesNotStarveBehindInvalidOwners(t *testing.T) {
	for _, outage := range []bool{false, true} {
		name := "ineligible"
		if outage {
			name = "directory-outage"
		}
		t.Run(name, func(t *testing.T) {
			db, _ := roleReminderMySQL(t)
			tenant := "tenant-reminder-starvation"
			for index, owner := range []string{"invalid-first", "invalid-second", "sales"} {
				snapshot, plan, contract := id(), id(), id()
				if _, err := db.Exec(`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,'test',?,?,?,1,'customer','测试客户','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, snapshot, tenant, id(), contract, contract); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO settlement_contract_responsibility(tenant_id,source_contract_id,owner_user_id,version,active,updated_at) VALUES(?,?,?,1,TRUE,UTC_TIMESTAMP(3))`, tenant, contract, owner); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,created_at,updated_at) VALUES(?,?,?,1,DATE_SUB(CURDATE(),INTERVAL ? DAY),100,'CONTRACT','PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, plan, tenant, snapshot, 5-index); err != nil {
					t.Fatal(err)
				}
			}
			directory := &starvationDirectory{outage: outage}
			worker := &RoleReminderWorker{DB: db, Service: &service.Service{DB: db}, TenantID: tenant, Directory: directory, BatchSize: 2}
			firstErr := worker.RunOnce(t.Context())
			if outage && firstErr == nil {
				t.Fatal("directory error lost")
			}
			if err := worker.RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			var actions int
			if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=? AND recipient_user_id='sales'`, tenant).Scan(&actions); err != nil || actions != 1 {
				t.Fatalf("later valid owner starved: actions=%d err=%v", actions, err)
			}
			var delayed int
			if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_role_reminder_cursor WHERE tenant_id=? AND owner_user_id<>'sales' AND next_at>UTC_TIMESTAMP(3) AND next_at<DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 1 HOUR)`, tenant).Scan(&delayed); err != nil || delayed != 2 {
				t.Fatalf("invalid owners lack finite retry: delayed=%d err=%v", delayed, err)
			}
			directory.recovered = true
			if _, err := db.Exec(`UPDATE settlement_role_reminder_cursor SET next_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 SECOND) WHERE tenant_id=? AND owner_user_id<>'sales'`, tenant); err != nil {
				t.Fatal(err)
			}
			if err := worker.RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=?`, tenant).Scan(&actions); err != nil || actions != 3 {
				t.Fatalf("recovered owners not retried: actions=%d err=%v", actions, err)
			}
		})
	}
}

// TestImportedFinanceHoldNoNotifications 验证历史逾期及待确认计划在重复扫描和重启时不催收。
// 参数：t为测试上下文，只运行显式隔离MySQL。
// 返回值：无，任一催收动作、通知或待发事件产生时失败。
func TestImportedFinanceHoldNoNotifications(t *testing.T) {
	db, _ := roleReminderMySQL(t)
	ctx := context.Background()
	tenant := "import-hold"
	for _, q := range []string{
		`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at,migration_state) VALUES('snapshot','import-hold','contract_management','event','source','IMPORT-1',1,'42','历史客户','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),'HOLD')`,
		`INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,created_at,updated_at) VALUES('confirmed','import-hold','snapshot',1,'2020-01-01',60,'CONTRACT_EVENT','CONFIRMED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3)),('pending','import-hold','snapshot',2,'2020-01-01',40,'CONTRACT_EVENT','PENDING_CONFIRMATION',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,receivable_plan_id,due_date,original_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES('receivable','import-hold','AR-1','snapshot','confirmed','2020-01-01',60,60,'CNY','CONFIRMED','UNPAID','NOT_INVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		`INSERT INTO settlement_contract_responsibility(tenant_id,source_contract_id,owner_user_id,version,active,updated_at) VALUES('import-hold','source','sales',1,TRUE,UTC_TIMESTAMP(3))`,
		`INSERT INTO settlement_dunning_policy(id,tenant_id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled,created_at,updated_at) VALUES('policy','import-hold','定制催收',1,1,36500,'REMIND','USER:collector','PLATFORM',1,'NORMAL',TRUE,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := (&Scanner{DB: db}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err := (&RoleReminderWorker{DB: db, TenantID: tenant, Service: &service.Service{DB: db}, Directory: &roleDirectoryFixture{}}).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"settlement_dunning_action", "settlement_local_notification"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE tenant_id=?", tenant).Scan(&n); err != nil || n != 0 {
			t.Fatalf("HOLD generated %s count=%d err=%v", table, n, err)
		}
	}
}
