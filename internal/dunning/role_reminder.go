package dunning

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
	"time"
)

type RoleReminderDirectory interface {
	ContainsRole(context.Context, string, string) (bool, error)
	ListRole(context.Context, string, string, int, int) ([]platform.Personnel, int64, error)
}
type RoleReminderWorker struct {
	DB                   *sql.DB
	TenantID             string
	Service              *service.Service
	Directory            RoleReminderDirectory
	CheckBusinessLicense func(context.Context) error
	BatchSize            int
}
type roleReminderCandidate struct {
	PlanID, SnapshotID, ReceivableID, Owner, Number, Customer string
	Aging                                                     int
	Pending                                                   bool
}

// reminderPhase 根据实际规则确定当前业务阶段。
// 参数：aging 逾期天数，pending 计划是否待确认，rules 已校验配置。
// 返回值：阶段；尚未到提醒日时为空。
func reminderPhase(aging int, pending bool, rules service.ReminderRules) string {
	if aging < -rules.LeadDays {
		return ""
	}
	if pending && aging <= 0 {
		return "PLAN_CONFIRMATION"
	}
	if aging >= rules.EscalationDays {
		return "ESCALATED"
	}
	if aging >= 1 {
		return "OVERDUE"
	}
	return "DUE_SOON"
}

// RunOnce 有界扫描当前租户负责人提醒并独立保留旧催收策略。
// 参数：ctx 后台任务上下文。
// 返回值：配置、目录或持久化错误；失败候选延期，其他候选继续处理。
func (w *RoleReminderWorker) RunOnce(ctx context.Context) error {
	if w.CheckBusinessLicense != nil {
		if err := w.CheckBusinessLicense(ctx); err != nil {
			return err
		}
	}
	if w.DB == nil || w.Service == nil || w.Directory == nil || w.TenantID == "" {
		return errors.New("role reminder requires tenant, database, rules and directory")
	}
	rules, err := w.Service.ReminderRules(ctx, w.TenantID)
	if err != nil {
		return err
	}
	batch := w.BatchSize
	if batch < 1 || batch > 100 {
		batch = 20
	}
	if err = w.closeRoleActions(ctx); err != nil {
		return err
	}
	query := `SELECT rp.id,cs.id,COALESCE(r.id,''),cr.owner_user_id,COALESCE(r.receivable_no,CONCAT('PLAN-',rp.installment_no)),cs.customer_name_snapshot,DATEDIFF(CURDATE(),rp.due_date),rp.confirmation_status<>'CONFIRMED'
 FROM settlement_receivable_plan rp JOIN settlement_contract_snapshot cs ON cs.id=rp.contract_snapshot_id AND cs.tenant_id=rp.tenant_id
 JOIN settlement_contract_responsibility cr ON cr.tenant_id=cs.tenant_id AND cr.source_contract_id=cs.source_contract_id AND cr.active=TRUE
 LEFT JOIN settlement_receivable r ON r.receivable_plan_id=rp.id AND r.tenant_id=rp.tenant_id
 LEFT JOIN settlement_role_reminder_cursor rc ON rc.tenant_id=rp.tenant_id AND rc.plan_id=rp.id AND rc.phase=CASE WHEN DATEDIFF(CURDATE(),rp.due_date)>=? THEN 'ESCALATED' WHEN rp.due_date<CURDATE() THEN 'OVERDUE' WHEN rp.confirmation_status<>'CONFIRMED' THEN 'PLAN_CONFIRMATION' ELSE 'DUE_SOON' END
 WHERE rp.tenant_id=? AND cs.financial_status='EFFECTIVE' AND rp.due_date<=DATE_ADD(CURDATE(),INTERVAL ? DAY)
 AND rp.planned_amount>0 AND (r.id IS NULL OR r.open_amount>0) AND (rc.next_at IS NULL OR rc.next_at<=UTC_TIMESTAMP(3) OR rc.owner_user_id<>cr.owner_user_id)
 AND NOT EXISTS(SELECT 1 FROM settlement_handover_freeze hf WHERE hf.tenant_id=rp.tenant_id AND hf.user_id=cr.owner_user_id AND hf.status='FROZEN')
 ORDER BY COALESCE(rc.next_at,'1000-01-01 00:00:00'),rp.due_date,rp.id LIMIT ?`
	rows, err := w.DB.QueryContext(ctx, query, rules.EscalationDays, w.TenantID, rules.LeadDays, batch)
	if err != nil {
		return err
	}
	items := []roleReminderCandidate{}
	for rows.Next() {
		var c roleReminderCandidate
		if err = rows.Scan(&c.PlanID, &c.SnapshotID, &c.ReceivableID, &c.Owner, &c.Number, &c.Customer, &c.Aging, &c.Pending); err != nil {
			rows.Close()
			return err
		}
		items = append(items, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, c := range items {
		if err = w.dispatch(ctx, c, rules); err != nil {
			failures = append(failures, err)
			if retryErr := w.deferCandidate(ctx, c, rules); retryErr != nil {
				failures = append(failures, retryErr)
			}
		}
	}
	return errors.Join(failures...)
}

// deferCandidate 为失败候选保存有限冷却期，避免队首反复失败阻塞后续计划。
// 参数：ctx 上下文，c 失败候选，rules 本轮经校验的提醒规则。
// 返回值：重试游标存储错误；责任或阶段变化时安全跳过。
func (w *RoleReminderWorker) deferCandidate(ctx context.Context, c roleReminderCandidate, rules service.ReminderRules) error {
	if w.CheckBusinessLicense != nil {
		if err := w.CheckBusinessLicense(ctx); err != nil {
			return err
		}
	}
	tx, err := w.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner string
	var aging int
	var pending bool
	err = tx.QueryRowContext(ctx, `SELECT cr.owner_user_id,DATEDIFF(CURDATE(),rp.due_date),rp.confirmation_status<>'CONFIRMED' FROM settlement_receivable_plan rp JOIN settlement_contract_snapshot cs ON cs.id=rp.contract_snapshot_id AND cs.tenant_id=rp.tenant_id JOIN settlement_contract_responsibility cr ON cr.tenant_id=cs.tenant_id AND cr.source_contract_id=cs.source_contract_id AND cr.active=TRUE WHERE rp.tenant_id=? AND rp.id=? FOR UPDATE`, w.TenantID, c.PlanID).Scan(&owner, &aging, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	phase := reminderPhase(aging, pending, rules)
	if owner != c.Owner || phase == "" || phase != reminderPhase(c.Aging, c.Pending, rules) {
		return nil
	}
	// 与成功派发复用计划行锁，不能把并发成功后的正常间隔缩短为故障重试间隔。
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_role_reminder_cursor(tenant_id,plan_id,phase,owner_user_id,next_at,updated_at) VALUES(?,?,?,?,DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 5 MINUTE),UTC_TIMESTAMP(3)) ON DUPLICATE KEY UPDATE next_at=IF(owner_user_id=VALUES(owner_user_id),GREATEST(next_at,VALUES(next_at)),VALUES(next_at)),owner_user_id=VALUES(owner_user_id),updated_at=UTC_TIMESTAMP(3)`, w.TenantID, c.PlanID, phase, owner)
	if err != nil {
		return err
	}
	if w.CheckBusinessLicense != nil {
		if err = w.CheckBusinessLicense(ctx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// closeRoleActions 取消已结清、责任失效或已确认计划的旧动作。
// 参数：ctx 工作上下文。
// 返回值：存储错误。
func (w *RoleReminderWorker) closeRoleActions(ctx context.Context) error {
	_, err := w.DB.ExecContext(ctx, `UPDATE settlement_dunning_action target JOIN (SELECT selected_id FROM (SELECT da.id AS selected_id FROM settlement_dunning_action da JOIN settlement_dunning_case dc ON dc.id=da.dunning_case_id AND dc.tenant_id=da.tenant_id JOIN settlement_dunning_policy dp ON dp.id=dc.dunning_policy_id AND dp.tenant_id=dc.tenant_id JOIN settlement_receivable_plan rp ON rp.id=dc.plan_id AND rp.tenant_id=dc.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=rp.contract_snapshot_id AND cs.tenant_id=rp.tenant_id LEFT JOIN settlement_receivable r ON r.receivable_plan_id=rp.id AND r.tenant_id=rp.tenant_id LEFT JOIN settlement_contract_responsibility cr ON cr.tenant_id=cs.tenant_id AND cr.source_contract_id=cs.source_contract_id AND cr.active=TRUE WHERE da.tenant_id=? AND dp.recipient_rule='CONTRACT_OWNER' AND da.status='PENDING' AND (r.open_amount=0 OR cr.owner_user_id IS NULL OR (da.action_type IN ('PLAN_CONFIRMATION','DUE_SOON','OVERDUE') AND da.recipient_user_id<>cr.owner_user_id) OR (da.action_type='PLAN_CONFIRMATION' AND rp.confirmation_status='CONFIRMED')) LIMIT 100) bounded_selection) due ON due.selected_id=target.id SET target.status='CANCELLED',target.result_detail='SOURCE_STATE_CHANGED',target.version=target.version+1,target.updated_at=UTC_TIMESTAMP(3)`, w.TenantID)
	if err != nil {
		return err
	}
	_, err = w.DB.ExecContext(ctx, `UPDATE settlement_dunning_case target JOIN (SELECT selected_id FROM (SELECT dc.id AS selected_id FROM settlement_dunning_case dc JOIN settlement_dunning_policy dp ON dp.id=dc.dunning_policy_id AND dp.tenant_id=dc.tenant_id JOIN settlement_receivable_plan rp ON rp.id=dc.plan_id AND rp.tenant_id=dc.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=rp.contract_snapshot_id AND cs.tenant_id=rp.tenant_id LEFT JOIN settlement_receivable r ON r.receivable_plan_id=rp.id AND r.tenant_id=rp.tenant_id LEFT JOIN settlement_contract_responsibility cr ON cr.tenant_id=cs.tenant_id AND cr.source_contract_id=cs.source_contract_id AND cr.active=TRUE WHERE dc.tenant_id=? AND dp.recipient_rule='CONTRACT_OWNER' AND dc.status='ACTIVE' AND (r.open_amount=0 OR cr.owner_user_id IS NULL) LIMIT 100) bounded_selection) due ON due.selected_id=target.id SET target.status='CLOSED',target.closed_reason='SOURCE_STATE_CHANGED',target.closed_at=UTC_TIMESTAMP(3),target.updated_at=UTC_TIMESTAMP(3)`, w.TenantID)
	return err
}

// escalationRecipients 读取有效的财务经理及管理层，目录超限时失败关闭。
// 参数：ctx 上下文。
// 返回值：去重用户ID或目录错误。
func (w *RoleReminderWorker) escalationRecipients(ctx context.Context) ([]string, error) {
	result := []string{}
	seen := map[string]bool{}
	for _, role := range []string{"settlement_finance_manager", "settlement_management"} {
		items, total, err := w.Directory.ListRole(ctx, role, "", 1, 100)
		if err != nil {
			return nil, err
		}
		if total > 100 || int64(len(items)) != total {
			return nil, errors.New("escalation personnel exceeds bounded directory result")
		}
		for _, person := range items {
			if person.UserID != "" && !seen[person.UserID] {
				result = append(result, person.UserID)
				seen[person.UserID] = true
			}
		}
	}
	return result, nil
}

// dispatch 重新锁定责任、计划和余额后原子创建提醒载体。
// 参数：ctx 上下文，c 候选计划，rules 本轮已校验规则。
// 返回值：目录、商业许可或存储错误；过期候选直接跳过。
func (w *RoleReminderWorker) dispatch(ctx context.Context, c roleReminderCandidate, rules service.ReminderRules) error {
	eligible, err := w.Directory.ContainsRole(ctx, c.Owner, "settlement_sales")
	if err != nil {
		return err
	}
	if !eligible {
		return errors.New("contract owner has no effective sales role")
	}
	phase := reminderPhase(c.Aging, c.Pending, rules)
	if phase == "" {
		return nil
	}
	recipients := []string{c.Owner}
	if phase == "ESCALATED" {
		extra, err := w.escalationRecipients(ctx)
		if err != nil {
			return err
		}
		for _, user := range extra {
			if user != c.Owner {
				recipients = append(recipients, user)
			}
		}
	}
	tx, err := w.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner string
	var aging int
	var pending, settled bool
	err = tx.QueryRowContext(ctx, `SELECT cr.owner_user_id,DATEDIFF(CURDATE(),rp.due_date),rp.confirmation_status<>'CONFIRMED',COALESCE(r.open_amount=0,FALSE) FROM settlement_receivable_plan rp JOIN settlement_contract_snapshot cs ON cs.id=rp.contract_snapshot_id AND cs.tenant_id=rp.tenant_id JOIN settlement_contract_responsibility cr ON cr.tenant_id=cs.tenant_id AND cr.source_contract_id=cs.source_contract_id AND cr.active=TRUE LEFT JOIN settlement_receivable r ON r.receivable_plan_id=rp.id AND r.tenant_id=rp.tenant_id WHERE rp.tenant_id=? AND rp.id=? AND cs.financial_status='EFFECTIVE' FOR UPDATE`, w.TenantID, c.PlanID).Scan(&owner, &aging, &pending, &settled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner != c.Owner || settled || reminderPhase(aging, pending, rules) != phase {
		return nil
	}
	var frozen bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settlement_handover_freeze WHERE tenant_id=? AND user_id=? AND status='FROZEN')`, w.TenantID, owner).Scan(&frozen); err != nil {
		return err
	}
	if frozen {
		return nil
	}
	// 计划行锁串行化首次游标写入，唯一键和再次检查使并发扫描不会重复派发。
	var previousOwner string
	var next time.Time
	err = tx.QueryRowContext(ctx, `SELECT owner_user_id,next_at FROM settlement_role_reminder_cursor WHERE tenant_id=? AND plan_id=? AND phase=? FOR UPDATE`, w.TenantID, c.PlanID, phase).Scan(&previousOwner, &next)
	if err == nil && previousOwner == owner && next.After(time.Now().UTC()) {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if w.CheckBusinessLicense != nil {
		if err = w.CheckBusinessLicense(ctx); err != nil {
			return err
		}
	}
	policyID := fmt.Sprintf("%x", sha256.Sum256([]byte(w.TenantID+":CONTRACT_OWNER")))[:32]
	_, err = tx.ExecContext(ctx, `INSERT IGNORE INTO settlement_dunning_policy(id,tenant_id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled,created_at,updated_at) VALUES(?,?,'系统合同负责人提醒',1,-36500,36500,'REMIND','CONTRACT_OWNER','LOCAL',?,'NORMAL',TRUE,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, policyID, w.TenantID, rules.RepeatDays)
	if err != nil {
		return err
	}
	caseID := id()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_dunning_case(id,tenant_id,receivable_id,plan_id,dunning_policy_id,policy_version,status,next_action_at,current_escalation_level,created_at,updated_at) VALUES(?,?,NULLIF(?,''),?,?,1,'ACTIVE',UTC_TIMESTAMP(3),0,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3)) ON DUPLICATE KEY UPDATE status='ACTIVE',receivable_id=VALUES(receivable_id),closed_reason='',closed_at=NULL`, caseID, w.TenantID, c.ReceivableID, c.PlanID, policyID)
	if err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_dunning_case WHERE tenant_id=? AND plan_id=? AND dunning_policy_id=? AND policy_version=1`, w.TenantID, c.PlanID, policyID).Scan(&caseID); err != nil {
		return err
	}
	for _, recipient := range recipients {
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settlement_handover_freeze WHERE tenant_id=? AND user_id=? AND status='FROZEN')`, w.TenantID, recipient).Scan(&frozen); err != nil {
			return err
		}
		if frozen {
			continue
		}
		if err = w.insertRoleAction(ctx, tx, c, caseID, recipient, phase); err != nil {
			return err
		}
	}
	repeat := rules.RepeatDays
	if phase == "DUE_SOON" {
		repeat = rules.LeadDays + 1
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_dunning_case SET next_action_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL ? DAY),last_action_at=UTC_TIMESTAMP(3),current_escalation_level=IF(?='ESCALATED',2,1),updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=?`, repeat, phase, w.TenantID, caseID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_role_reminder_cursor(tenant_id,plan_id,phase,owner_user_id,next_at,updated_at) VALUES(?,?,?,?,DATE_ADD(UTC_TIMESTAMP(3),INTERVAL ? DAY),UTC_TIMESTAMP(3)) ON DUPLICATE KEY UPDATE owner_user_id=VALUES(owner_user_id),next_at=VALUES(next_at),updated_at=UTC_TIMESTAMP(3)`, w.TenantID, c.PlanID, phase, owner, repeat)
	if err != nil {
		return err
	}
	if w.CheckBusinessLicense != nil {
		if err = w.CheckBusinessLicense(ctx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// insertRoleAction 原子写入个人跟进动作、站内通知和平台审计事件。
// 参数：ctx 上下文，tx 事务，c 计划，caseID 案件，recipient 有效接收者，phase 阶段。
// 返回值：持久化错误。
func (w *RoleReminderWorker) insertRoleAction(ctx context.Context, tx *sql.Tx, c roleReminderCandidate, caseID, recipient, phase string) error {
	eventID, actionID := id(), id()
	resourceType := "dunning_action"
	target := "/settlement/dunning"
	if recipient != c.Owner {
		actionID = caseID
		resourceType = "dunning_case"
		target = "/settlement"
	}
	priority := "NORMAL"
	title := "合同回款计划提醒"
	if phase == "ESCALATED" {
		priority = "HIGH"
		title = "合同回款逾期升级"
	}
	content := fmt.Sprintf("%s，%s：阶段%s，距到期天数%d。请打开结算跟进并记录处理结果。", c.Customer, c.Number, phase, -c.Aging)
	var err error
	if recipient == c.Owner {
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_dunning_action(id,tenant_id,dunning_case_id,action_type,recipient_user_id,channel,priority,event_id,status,version,sent_at,updated_at,created_at) VALUES(?,?,?,?,?,'LOCAL',?,?,'PENDING',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, actionID, w.TenantID, caseID, phase, recipient, priority, eventID)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_local_notification(id,tenant_id,recipient_user_id,event_id,notification_scope,priority,title,content,target_url,reference_type,reference_id,created_at) VALUES(?,?,?,?,'LOCAL',?,?,?,?,'RECEIVABLE_PLAN',?,UTC_TIMESTAMP(3))`, id(), w.TenantID, recipient, eventID, priority, title, content, target, c.PlanID)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"event_id": eventID, "event_type": "SETTLEMENT_OWNER_REMINDER", "recipient_user_ids": []string{recipient}, "title": title, "content": content, "priority": priority, "notification_scope": "CROSS_SYSTEM", "target_url": target, "reference_type": "RECEIVABLE_PLAN", "reference_id": c.PlanID, "idempotency_key": eventID, "occurred_at": time.Now().UTC().Format(time.RFC3339Nano)})
	if phase == "ESCALATED" {
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_outbox_event(id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,status,available_at,created_at,updated_at) VALUES(?,?,?,'PLATFORM_NOTIFICATION','SETTLEMENT_OWNER_REMINDER',?,?,?,'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id(), w.TenantID, eventID, resourceType, actionID, payload)
		if err != nil {
			return err
		}
	}
	audit, _ := json.Marshal(map[string]any{"actor_id": "system:role-reminder", "result": "SUCCESS", "phase": phase, "action_id": actionID, "recipient_user_id": recipient})
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_outbox_event(id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,status,available_at,created_at,updated_at) VALUES(?,?,?,'PLATFORM_AUDIT','SETTLEMENT_OWNER_REMINDER_CREATED',?,?,?,'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id(), w.TenantID, id(), resourceType, actionID, audit)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_audit_event(id,tenant_id,actor_id,action,resource_type,resource_id,result,reason_code,request_id,correlation_id,detail_json,occurred_at) VALUES(?,?,'system:role-reminder','SETTLEMENT_OWNER_REMINDER_CREATED',?,?,'SUCCESS','','','',?,UTC_TIMESTAMP(3))`, id(), w.TenantID, resourceType, actionID, audit)
	return err
}
