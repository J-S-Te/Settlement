package dunning

import (
	"context"
	"database/sql"
	"sync"
	"testing"
)

// activeCaseFixture 在真实迁移的隔离库中建立一条可催收的应收、回款计划及两条策略。
// 参数：t 测试上下文，db 隔离库。
// 返回值：租户、合同快照、应收、计划及两条策略标识。
func activeCaseFixture(t *testing.T, db *sql.DB) (tenant, snapshot, plan, receivable, firstPolicy, secondPolicy string) {
	t.Helper()
	tenant, snapshot, plan, receivable = "tenant-active-case", id(), id(), id()
	firstPolicy, secondPolicy = id(), id()
	steps := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,'test',?,'contract','C001',1,'customer','唯一案件客户','CNY',100,'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{snapshot, tenant, id()}},
		{`INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,created_at,updated_at) VALUES(?,?,?,1,DATE_SUB(CURDATE(),INTERVAL 5 DAY),100,'CONTRACT','CONFIRMED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{plan, tenant, snapshot}},
		{`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,receivable_plan_id,due_date,original_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,?,'AR-UNIQUE',?,?,DATE_SUB(CURDATE(),INTERVAL 5 DAY),100,100,'CNY','CONFIRMED','UNPAID','NOT_INVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{receivable, tenant, snapshot, plan}},
		{`INSERT INTO settlement_dunning_policy(id,tenant_id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled,created_at,updated_at) VALUES(?,?,'自定义策略甲',1,1,30,'REMIND','USER:collector','LOCAL',7,'NORMAL',TRUE,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{firstPolicy, tenant}},
		{`INSERT INTO settlement_dunning_policy(id,tenant_id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled,created_at,updated_at) VALUES(?,?,'自定义策略乙',1,5,60,'REMIND','USER:collector','LOCAL',7,'NORMAL',TRUE,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{secondPolicy, tenant}},
	}
	for _, step := range steps {
		if _, err := db.Exec(step.query, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	return tenant, snapshot, plan, receivable, firstPolicy, secondPolicy
}

// insertActiveCase 直接写入 ACTIVE 催收案件，用于验证数据库级唯一不变量。
// 参数：t 测试上下文，db 隔离库，tenant 租户，policy 策略，receivable/plan 可为空字符串。
// 返回值：写入错误供断言唯一键冲突。
func insertActiveCase(t *testing.T, db *sql.DB, tenant, policy, receivable, plan string) error {
	t.Helper()
	_, err := db.Exec(`INSERT INTO settlement_dunning_case(id,tenant_id,receivable_id,plan_id,dunning_policy_id,policy_version,current_escalation_level,status,next_action_at,last_action_at,created_at,updated_at)
VALUES(?,?,NULLIF(?,''),NULLIF(?,''),?,1,0,'ACTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id(), tenant, receivable, plan, policy)
	return err
}

// TestMySQLDunningActiveCaseUniqueConstraint 验证迁移交付的生成列唯一键：
// 同一应收和同一计划最多一个 ACTIVE 案件，关闭后可再次立案，历史 CLOSED 案件不被约束。
// 参数：t 测试上下文；仅显式隔离 SETTLEMENT_TEST_DSN 运行。
// 返回值：无。
func TestMySQLDunningActiveCaseUniqueConstraint(t *testing.T) {
	db, _ := roleReminderMySQL(t)
	tenant, _, plan, receivable, firstPolicy, secondPolicy := activeCaseFixture(t, db)
	if err := insertActiveCase(t, db, tenant, firstPolicy, receivable, ""); err != nil {
		t.Fatal(err)
	}
	// 同一应收的第二条 ACTIVE 案件必须被数据库拒绝，即使策略不同。
	if err := insertActiveCase(t, db, tenant, secondPolicy, receivable, ""); !duplicateKeyError(err) {
		t.Fatalf("second active case for receivable err=%v want duplicate key", err)
	}
	// 同一回款计划的第二条 ACTIVE 案件同样被拒绝（应收为空，覆盖计划维度）。
	if err := insertActiveCase(t, db, tenant, firstPolicy, "", plan); err != nil {
		t.Fatal(err)
	}
	if err := insertActiveCase(t, db, tenant, secondPolicy, "", plan); !duplicateKeyError(err) {
		t.Fatalf("second active case for plan err=%v want duplicate key", err)
	}
	// 关闭后允许重新立案；历史 CLOSED 行不受唯一键约束。
	if _, err := db.Exec(`UPDATE settlement_dunning_case SET status='CLOSED',closed_reason='RECEIVABLE_SETTLED',closed_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND active_receivable_id IS NOT NULL`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := insertActiveCase(t, db, tenant, secondPolicy, receivable, ""); err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	// 预检 SQL 必须是可执行且只读的；干净数据下返回空集。
	prechecks := []string{
		`SELECT tenant_id,receivable_id,COUNT(*) AS active_cases FROM settlement_dunning_case WHERE status='ACTIVE' AND receivable_id IS NOT NULL GROUP BY tenant_id,receivable_id HAVING COUNT(*)>1`,
		`SELECT tenant_id,plan_id,COUNT(*) AS active_cases FROM settlement_dunning_case WHERE status='ACTIVE' AND plan_id IS NOT NULL GROUP BY tenant_id,plan_id HAVING COUNT(*)>1`,
	}
	for _, query := range prechecks {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if rows.Next() {
			t.Fatal("precheck reported duplicate active cases after migration")
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMySQLDunningConcurrentScannersKeepOneActiveCase 验证两个扫描器并发立案时，
// 数据库唯一键与 INSERT IGNORE 让位共同保证同一应收只有一个 ACTIVE 案件和一套动作。
// 参数：t 测试上下文；仅显式隔离 SETTLEMENT_TEST_DSN 运行。
// 返回值：无。
func TestMySQLDunningConcurrentScannersKeepOneActiveCase(t *testing.T) {
	db, _ := roleReminderMySQL(t)
	tenant, _, _, _, _, _ := activeCaseFixture(t, db)
	scanner := &Scanner{DB: db, BatchSize: 50, Directory: collectorStub{eligible: true}}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- scanner.RunOnce(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var cases, active, actions int
	if err := db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(status='ACTIVE'),0) FROM settlement_dunning_case WHERE tenant_id=?`, tenant).Scan(&cases, &active); err != nil || cases != 1 || active != 1 {
		t.Fatalf("concurrent scanners cases=%d active=%d err=%v", cases, active, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_dunning_action WHERE tenant_id=?`, tenant).Scan(&actions); err != nil || actions != 1 {
		t.Fatalf("concurrent scanners actions=%d err=%v", actions, err)
	}
}

// TestMySQLDunningReopenKeepsOneActiveCase 验证应收恢复欠款时，即使历史同一应收
// 有多条已结清案件，重开也只产生一个 ACTIVE 案件且整批扫描不因唯一键失败。
// 参数：t 测试上下文；仅显式隔离 SETTLEMENT_TEST_DSN 运行。
// 返回值：无。
func TestMySQLDunningReopenKeepsOneActiveCase(t *testing.T) {
	db, _ := roleReminderMySQL(t)
	tenant, _, _, receivable, firstPolicy, secondPolicy := activeCaseFixture(t, db)
	for _, policy := range []string{firstPolicy, secondPolicy} {
		_, err := db.Exec(`INSERT INTO settlement_dunning_case(id,tenant_id,receivable_id,plan_id,dunning_policy_id,policy_version,current_escalation_level,status,next_action_at,last_action_at,closed_reason,closed_at,created_at,updated_at)
VALUES(?,?,?,NULL,?,1,0,'CLOSED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),'RECEIVABLE_SETTLED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id(), tenant, receivable, policy)
		if err != nil {
			t.Fatal(err)
		}
	}
	scanner := &Scanner{DB: db, BatchSize: 50, Directory: collectorStub{eligible: true}}
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var cases, active int
	if err := db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(status='ACTIVE'),0) FROM settlement_dunning_case WHERE tenant_id=?`, tenant).Scan(&cases, &active); err != nil || cases != 2 || active != 1 {
		t.Fatalf("reopen cases=%d active=%d err=%v", cases, active, err)
	}
}
