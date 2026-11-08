package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

type creditFlowReceiver struct {
	mu                  sync.Mutex
	accepted            map[string]map[string]any
	order               []string
	loseAcknowledgement bool
}

// Publish 模拟CRM幂等受理与原付款存在约束，并可在受理后丢失一次ACK。
// 参数：ctx为取消上下文，tenant/eventID为可信身份，payload为正式服务生成的事件。
// 返回值：ACK丢失或未收到原付款时返回错误，其余受理成功。
func (r *creditFlowReceiver) Publish(ctx context.Context, tenant, eventID string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return err
	}
	if r.accepted == nil {
		r.accepted = map[string]map[string]any{}
	}
	if _, ok := r.accepted[eventID]; ok {
		return nil
	}
	if event["event_type"] == creditEventReversed {
		found := false
		for _, original := range r.accepted {
			if original["payment_id"] == event["reversal_of_payment_id"] {
				found = true
			}
		}
		if !found {
			return errors.New("original payment not accepted")
		}
	}
	r.accepted[eventID] = event
	r.order = append(r.order, eventID)
	if r.loseAcknowledgement {
		r.loseAcknowledgement = false
		return errors.New("CRM accepted payment but acknowledgement was lost")
	}
	return nil
}

// seedCreditFlowPlan 建立有真实CRM客户引用的100元合同及已确认计划。
// 参数：t为测试上下文，db为独占测试库，tenant为本轮合成租户。
// 返回值：正式服务可消费的应收与计划标识。
func seedCreditFlowPlan(t *testing.T, db *sql.DB, tenant string) (string, string) {
	t.Helper()
	snapshot, plan, receivable := newID(), newID(), newID()
	steps := []struct {
		q string
		a []any
	}{
		{`INSERT INTO settlement_contract_snapshot
   (id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,
    source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,
    financial_status,effective_at,created_at,updated_at)
   VALUES(?,?,'contract_management',?,?,'C-CREDIT',1,'42','隔离信用客户','CNY',100,
    'EFFECTIVE',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{snapshot, tenant, newID(), newID()}},
		{`INSERT INTO settlement_receivable_plan
   (id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,
    generation_source,confirmation_status,created_at,updated_at)
   VALUES(?,?,?,1,'2026-12-01',100,'CONTRACT_EVENT','CONFIRMED',
    UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{plan, tenant, snapshot}},
		{`INSERT INTO settlement_receivable
   (id,tenant_id,receivable_no,contract_snapshot_id,receivable_plan_id,due_date,
    original_amount,open_amount,currency,recognition_status,collection_status,invoice_status,
    created_at,updated_at)
   VALUES(?,?,?,?,?,'2026-12-01',100,100,'CNY','CONFIRMED','UNPAID','UNINVOICED',
    UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, []any{receivable, tenant, newID(), snapshot, plan}},
	}
	for _, step := range steps {
		if _, err := db.ExecContext(t.Context(), step.q, step.a...); err != nil {
			t.Fatal(err)
		}
	}
	return receivable, plan
}

// allocateCreditFlow 通过正式回款与核销服务形成指定金额的真实财务事实。
// 参数：t为测试上下文，svc为服务，p为租户主体，receivable/value为核销目标及金额。
// 返回值：正式核销标识；任何业务失败终止测试。
func allocateCreditFlow(t *testing.T, svc *Service, p Principal, receivable, value string) string {
	t.Helper()
	receipt, err := svc.CreateReceipt(t.Context(), p, newID(), ReceiptInput{
		CustomerID: "42", CustomerName: "隔离信用客户", Amount: value, Currency: "CNY",
		ReceiptDate: "2026-11-01", PaymentMethod: "TRANSFER", BankReference: newID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := svc.ConfirmAllocation(t.Context(), p, newID(), AllocationInput{
		ReceiptID: receipt, ReceivableID: receivable, Amount: value, MatchMode: "MANUAL",
	})
	if err != nil {
		t.Fatal(err)
	}
	return allocation
}

// assertCreditFlowState 校验隔离数据库内的真实应收金额和收款状态。
// 参数：t/db为测试与连接，tenant/id为应收归属，original/open/status为独立预期。
// 返回值：无；金额或状态不符时失败。
func assertCreditFlowState(t *testing.T, db *sql.DB, tenant, id, original, open, status string) {
	t.Helper()
	var gotOriginal, gotOpen, gotStatus string
	err := db.QueryRowContext(t.Context(), `SELECT original_amount,open_amount,collection_status
  FROM settlement_receivable WHERE tenant_id=? AND id=?`, tenant, id).
		Scan(&gotOriginal, &gotOpen, &gotStatus)
	if err != nil || gotOriginal != original || gotOpen != open || gotStatus != status {
		t.Fatalf("应收=%s/%s/%s，期望%s/%s/%s，err=%v", gotOriginal, gotOpen, gotStatus, original, open, status, err)
	}
}

// TestMySQLCreditAcknowledgementLossCompensatesInOrder 验证ACK丢失后冲销保留原事实并有序补偿。
// 参数：t为测试上下文，仅使用显式隔离DSN新建独占库。
// 返回值：无；原事实丢失、补偿提前或账务不一致时失败。
func TestMySQLCreditAcknowledgementLossCompensatesInOrder(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	p := Principal{TenantID: newID(), UserID: "confirmer", Permissions: map[string]bool{"settlement.admin": true}}
	receiver := &creditFlowReceiver{loseAcknowledgement: true}
	svc := &Service{DB: db, CreditPublisher: receiver}
	receivable, _ := seedCreditFlowPlan(t, db, p.TenantID)
	allocation := allocateCreditFlow(t, svc, p, receivable, "100.00")
	var completed string
	if err := db.QueryRow(`SELECT event_id FROM settlement_credit_sync WHERE tenant_id=?`, p.TenantID).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	approver := p
	approver.UserID = "independent-approver"
	if _, err := svc.ReverseAllocation(t.Context(), approver, newID(), allocation, ReversalInput{Amount: "10", ReasonCode: "ACK_LOSS"}); err != nil {
		t.Fatal(err)
	}
	assertCreditFlowState(t, db, p.TenantID, receivable, "100.00", "10.00", "PARTIALLY_SETTLED")
	var status, reversed string
	if err := db.QueryRow(`SELECT status FROM settlement_credit_sync WHERE event_id=?`, completed).Scan(&status); err != nil || status != "RETRY_WAIT" {
		t.Fatalf("原结清必须保留，status=%s err=%v", status, err)
	}
	if err := db.QueryRow(`SELECT event_id FROM settlement_credit_sync WHERE tenant_id=? AND event_type=?`, p.TenantID, creditEventReversed).Scan(&reversed); err != nil {
		t.Fatalf("丢ACK后缺少补偿: %v", err)
	}
	if len(receiver.order) != 1 {
		t.Fatalf("确认原事实前不应提前发补偿: %v", receiver.order)
	}
	worker := &CreditSyncWorker{DB: db, Publisher: receiver, BatchSize: 10}
	for i := 0; i < 2; i++ {
		if err := worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(`SELECT status FROM settlement_credit_sync WHERE event_id=?`, reversed).Scan(&status); err != nil || status != "DELIVERED" {
		t.Fatalf("补偿未完成 status=%s err=%v", status, err)
	}
	if len(receiver.order) != 2 || receiver.order[0] != completed || receiver.order[1] != reversed {
		t.Fatalf("事实受理顺序=%v", receiver.order)
	}
}

// TestMySQLCreditPlanAdjustmentTransitions 验证96结清调增100再调回96时信用事实按轮次转换。
// 参数：t为测试上下文，仅使用显式隔离DSN新建独占库。
// 返回值：无；漏补偿、复用旧金额或新事实越过前轮撤销时失败。
func TestMySQLCreditPlanAdjustmentTransitions(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	p := Principal{TenantID: newID(), UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	receiver := &creditFlowReceiver{}
	svc := &Service{DB: db, CreditPublisher: receiver}
	receivable, plan := seedCreditFlowPlan(t, db, p.TenantID)
	if err := svc.AdjustPlan(t.Context(), p, plan, PlanConfirmationInput{Version: 1, DueDate: "2026-12-01", PlannedAmount: "96", Reason: "容差内调整"}); err != nil {
		t.Fatal(err)
	}
	allocateCreditFlow(t, svc, p, receivable, "96.00")
	assertCreditFlowState(t, db, p.TenantID, receivable, "96.00", "0.00", "SETTLED")
	// 暂停即时发送，仅保留正式队列，验证补偿与后续完成的前驱依赖。
	svc.CreditPublisher = nil
	if err := svc.AdjustPlan(t.Context(), p, plan, PlanConfirmationInput{Version: 2, DueDate: "2026-12-02", PlannedAmount: "100", Reason: "恢复应收"}); err != nil {
		t.Fatal(err)
	}
	assertCreditFlowState(t, db, p.TenantID, receivable, "100.00", "4.00", "PARTIALLY_SETTLED")
	var reversed string
	if err := db.QueryRow(`SELECT event_id FROM settlement_credit_sync WHERE tenant_id=? AND event_type=?`, p.TenantID, creditEventReversed).Scan(&reversed); err != nil {
		t.Fatalf("调整离开结清没有补偿: %v", err)
	}
	if err := svc.AdjustPlan(t.Context(), p, plan, PlanConfirmationInput{Version: 3, DueDate: "2026-12-03", PlannedAmount: "96", Reason: "重新确认计划"}); err != nil {
		t.Fatal(err)
	}
	var completed, predecessor string
	var payload []byte
	if err := db.QueryRow(`SELECT event_id,original_event_id,payload_json FROM settlement_credit_sync
  WHERE tenant_id=? AND event_type=? AND original_event_id=?`, p.TenantID, creditEventCompleted, reversed).Scan(&completed, &predecessor, &payload); err != nil {
		t.Fatalf("新结清缺少撤销前驱: %v", err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{"due_amount": "96.00", "paid_amount": "96.00", "due_date": "2026-12-03T00:00:00Z", "paid_date": "2026-11-01T00:00:00Z", "payment_id": completed} {
		if event[field] != want {
			t.Fatalf("%s=%v，期望%s", field, event[field], want)
		}
	}
	svc.CreditPublisher = receiver
	if err := svc.RetryCreditSync(t.Context(), p.TenantID, completed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("撤销前新完成不应可认领: %v", err)
	}
	worker := &CreditSyncWorker{DB: db, Publisher: receiver, BatchSize: 10}
	for i := 0; i < 2; i++ {
		if err := worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(receiver.order) != 3 || receiver.order[1] != reversed || receiver.order[2] != completed {
		t.Fatalf("转换事实顺序=%v", receiver.order)
	}
	assertCreditFlowState(t, db, p.TenantID, receivable, "96.00", "0.00", "SETTLED")
	if err := svc.AdjustPlan(t.Context(), p, plan, PlanConfirmationInput{Version: 4,
		DueDate: "2026-12-04", PlannedAmount: "100", Reason: "核对新付款撤销身份"}); err != nil {
		t.Fatal(err)
	}
	var reversedPayload []byte
	if err := db.QueryRow(`SELECT payload_json FROM settlement_credit_sync
	 WHERE tenant_id=? AND event_type=? AND original_event_id=?`,
		p.TenantID, creditEventReversed, completed).Scan(&reversedPayload); err != nil {
		t.Fatal(err)
	}
	var reversal map[string]any
	if err := json.Unmarshal(reversedPayload, &reversal); err != nil {
		t.Fatal(err)
	}
	if reversal["reversal_of_payment_id"] != completed {
		t.Fatalf("调整后付款撤销身份=%v，期望新事件身份%s", reversal, completed)
	}
}

// TestMySQLCreditPlanConcurrentAdjustmentCompensatesOnce 验证同版本并发调整只产生一次撤销。
// 参数：t为测试上下文，测试仅使用独占隔离库及正式财务服务。
// 返回值：无；版本门禁失效、重复补偿或失败事务污染账务时失败。
func TestMySQLCreditPlanConcurrentAdjustmentCompensatesOnce(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	p := Principal{TenantID: newID(), UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	svc := &Service{DB: db, CreditPublisher: &creditFlowReceiver{}}
	receivable, plan := seedCreditFlowPlan(t, db, p.TenantID)
	if err := svc.AdjustPlan(t.Context(), p, plan, PlanConfirmationInput{Version: 1, DueDate: "2026-12-01", PlannedAmount: "96", Reason: "初始计划"}); err != nil {
		t.Fatal(err)
	}
	allocateCreditFlow(t, svc, p, receivable, "96")
	svc.CreditPublisher = nil
	start := make(chan struct{})
	outcomes := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			outcomes <- svc.AdjustPlan(t.Context(), p, plan, PlanConfirmationInput{Version: 2, DueDate: "2026-12-02", PlannedAmount: "100", Reason: "并发恢复应收"})
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-outcomes
		if err == nil {
			successes++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	var reversals int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_credit_sync WHERE tenant_id=? AND event_type=?`, p.TenantID, creditEventReversed).Scan(&reversals); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 || reversals != 1 {
		t.Fatalf("成功=%d 冲突=%d 补偿=%d", successes, conflicts, reversals)
	}
	assertCreditFlowState(t, db, p.TenantID, receivable, "100.00", "4.00", "PARTIALLY_SETTLED")
}

// TestMySQLCreditCancelledCompletionKeepsPredecessor 验证取消未发送完成后仍等待前轮撤销。
// 参数：t为测试上下文，所有计划变化通过正式AdjustPlan写入独占库。
// 返回值：无；新完成绕过未投递撤销或取消事件被发送时失败。
func TestMySQLCreditCancelledCompletionKeepsPredecessor(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	p := Principal{TenantID: newID(), UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	receiver := &creditFlowReceiver{}
	svc := &Service{DB: db, CreditPublisher: receiver}
	receivable, plan := seedCreditFlowPlan(t, db, p.TenantID)
	for i, value := range []string{"96", "100", "96", "100", "96"} {
		if err := svc.AdjustPlan(t.Context(), p, plan, PlanConfirmationInput{Version: i + 1, DueDate: "2026-12-01", PlannedAmount: value, Reason: "验证多轮顺序"}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			allocateCreditFlow(t, svc, p, receivable, "96")
			svc.CreditPublisher = nil
		}
	}
	var completed, predecessor, reversed string
	if err := db.QueryRow(`SELECT event_id,original_event_id FROM settlement_credit_sync
  WHERE tenant_id=? AND event_type=? AND status='PENDING'`, p.TenantID, creditEventCompleted).Scan(&completed, &predecessor); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT event_id FROM settlement_credit_sync WHERE tenant_id=? AND event_type=?`, p.TenantID, creditEventReversed).Scan(&reversed); err != nil {
		t.Fatal(err)
	}
	if predecessor != reversed {
		t.Fatalf("前驱=%s，期望尚未发送的撤销%s", predecessor, reversed)
	}
	svc.CreditPublisher = receiver
	if err := svc.RetryCreditSync(t.Context(), p.TenantID, completed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("取消后的新完成越过前轮撤销: %v", err)
	}
	worker := &CreditSyncWorker{DB: db, Publisher: receiver}
	for i := 0; i < 2; i++ {
		if err := worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var cancelled int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_credit_sync WHERE tenant_id=? AND status='CANCELLED'`, p.TenantID).Scan(&cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled != 1 || len(receiver.order) != 3 || receiver.order[1] != reversed || receiver.order[2] != completed {
		t.Fatalf("取消=%d 受理顺序=%v", cancelled, receiver.order)
	}
}

// TestMySQLCreditAttemptedPendingConcurrentDelivery 验证尝试证据阻止取消并保持并发投递顺序。
// 参数：t为测试上下文，独占库内模拟旧状态恢复为PENDING且保留发送证据。
// 返回值：无；已尝试事实被取消、并发重复受理或撤销提前时失败。
func TestMySQLCreditAttemptedPendingConcurrentDelivery(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	p := Principal{TenantID: newID(), UserID: "confirmer", Permissions: map[string]bool{"settlement.admin": true}}
	receiver := &creditFlowReceiver{loseAcknowledgement: true}
	svc := &Service{DB: db, CreditPublisher: receiver}
	receivable, _ := seedCreditFlowPlan(t, db, p.TenantID)
	allocation := allocateCreditFlow(t, svc, p, receivable, "100")
	var completed string
	if err := db.QueryRow(`SELECT event_id FROM settlement_credit_sync WHERE tenant_id=?`, p.TenantID).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE settlement_credit_sync SET status='PENDING' WHERE tenant_id=?`, p.TenantID); err != nil {
		t.Fatal(err)
	}
	approver := p
	approver.UserID = "approver"
	if _, err := svc.ReverseAllocation(t.Context(), approver, newID(), allocation, ReversalInput{Amount: "10", ReasonCode: "ATTEMPTED_PENDING"}); err != nil {
		t.Fatal(err)
	}
	var reversed string
	if err := db.QueryRow(`SELECT event_id FROM settlement_credit_sync WHERE tenant_id=? AND event_type=?`, p.TenantID, creditEventReversed).Scan(&reversed); err != nil {
		t.Fatalf("有尝试证据的PENDING缺少补偿: %v", err)
	}
	start := make(chan struct{})
	outcomes := make(chan error, 3)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			worker := &CreditSyncWorker{DB: db, Publisher: receiver}
			outcomes <- worker.RunOnce(t.Context())
		}()
	}
	go func() { <-start; outcomes <- svc.RetryCreditSync(t.Context(), p.TenantID, completed) }()
	close(start)
	for i := 0; i < 3; i++ {
		if err := <-outcomes; err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	worker := &CreditSyncWorker{DB: db, Publisher: receiver}
	for i := 0; i < 2; i++ {
		if err := worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var delivered int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_credit_sync WHERE tenant_id=? AND status='DELIVERED'`, p.TenantID).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 2 || len(receiver.order) != 2 || receiver.order[0] != completed || receiver.order[1] != reversed {
		t.Fatalf("已确认=%d 外部受理=%v", delivered, receiver.order)
	}
}

// TestMySQLCreditPausedCompensationResumes 验证暂停期间保留补偿意图并在恢复订阅后有序发送。
// 参数：t为测试上下文，覆盖未知ACK、过期在途以及已确认事实的正式账务变更。
// 返回值：无；暂停期间发送、丢失补偿或恢复后信用事实不完整时失败。
func TestMySQLCreditPausedCompensationResumes(t *testing.T) {
	for _, state := range []string{"RETRY_WAIT", "PROCESSING", "DELIVERED"} {
		t.Run(state, func(t *testing.T) {
			db, _ := invoiceArchiveMySQL(t)
			p := Principal{TenantID: newID(), UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
			receiver := &creditFlowReceiver{loseAcknowledgement: state != "DELIVERED"}
			svc := &Service{DB: db, CreditPublisher: receiver}
			svc.ConfigureFinancialSources(true, true)
			receivable, plan := seedCreditFlowPlan(t, db, p.TenantID)
			value := "100"
			if state == "DELIVERED" {
				value = "96"
				if err := svc.AdjustPlan(t.Context(), p, plan, PlanConfirmationInput{Version: 1, DueDate: "2026-12-01", PlannedAmount: value, Reason: "建立结清计划"}); err != nil {
					t.Fatal(err)
				}
			}
			allocation := allocateCreditFlow(t, svc, p, receivable, value)
			var completed string
			if err := db.QueryRow(`SELECT event_id FROM settlement_credit_sync WHERE tenant_id=?`, p.TenantID).Scan(&completed); err != nil {
				t.Fatal(err)
			}
			if state == "PROCESSING" {
				if _, err := db.Exec(`UPDATE settlement_credit_sync SET status='PROCESSING',
     locked_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 10 MINUTE) WHERE tenant_id=?`, p.TenantID); err != nil {
					t.Fatal(err)
				}
			}
			svc.ConfigureFinancialSources(true, false)
			if state == "DELIVERED" {
				if err := svc.AdjustPlan(t.Context(), p, plan, PlanConfirmationInput{Version: 2, DueDate: "2026-12-02", PlannedAmount: "100", Reason: "暂停期间调增应收"}); err != nil {
					t.Fatal(err)
				}
				assertCreditFlowState(t, db, p.TenantID, receivable, "100.00", "4.00", "PARTIALLY_SETTLED")
			} else {
				approver := p
				approver.UserID = "independent-approver"
				if _, err := svc.ReverseAllocation(t.Context(), approver, newID(), allocation, ReversalInput{Amount: "10", ReasonCode: "PAUSED_COMPENSATION"}); err != nil {
					t.Fatal(err)
				}
				assertCreditFlowState(t, db, p.TenantID, receivable, "100.00", "10.00", "PARTIALLY_SETTLED")
			}
			var reversed string
			if err := db.QueryRow(`SELECT event_id FROM settlement_credit_sync WHERE tenant_id=?
    AND event_type=? AND original_event_id=?`, p.TenantID, creditEventReversed, completed).Scan(&reversed); err != nil {
				t.Fatalf("暂停期间丢失必要补偿意图: %v", err)
			}
			if err := svc.RetryCreditSync(t.Context(), p.TenantID, reversed); !errors.Is(err, ErrCreditSyncNotSubscribed) {
				t.Fatalf("暂停人工投递门禁=%v", err)
			}
			worker := &CreditSyncWorker{DB: db, Publisher: receiver, SubscriptionDisabled: true}
			if err := worker.RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(receiver.order) != 1 {
				t.Fatalf("暂停期间不得发送补偿: %v", receiver.order)
			}
			svc.ConfigureFinancialSources(true, true)
			worker.SubscriptionDisabled = false
			for i := 0; i < 2; i++ {
				if err := worker.RunOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			var delivered int
			if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_credit_sync WHERE tenant_id=? AND status='DELIVERED'`, p.TenantID).Scan(&delivered); err != nil {
				t.Fatal(err)
			}
			if delivered != 2 || len(receiver.order) != 2 || receiver.order[0] != completed || receiver.order[1] != reversed {
				t.Fatalf("恢复后确认=%d 受理顺序=%v", delivered, receiver.order)
			}
		})
	}
}
