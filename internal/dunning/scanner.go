package dunning

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PersonnelDirectory 是分配催收责任前使用的窄范围
// 目录检查。它可选，使本地扫描器测试无需平台
// 目录；生产装配必须提供。
type PersonnelDirectory interface {
	ContainsCollector(ctx context.Context, userID string) (bool, error)
}

type Scanner struct {
	CheckBusinessLicense func(context.Context) error
	DB                   *sql.DB
	BatchSize            int
	Directory            PersonnelDirectory
	Now                  func() time.Time
}

type candidate struct {
	TenantID, ReceivableID, Number, Customer, PolicyID, ActionType, RecipientRule, Channel, Priority string
	PolicyVersion, RepeatDays, AgingDays                                                             int
}

type dueCase struct {
	TenantID, CaseID, PolicyID, ReceivableID, Number, Customer, ActionType, Recipient, Channel, Priority string
	PolicyVersion, Level, RepeatDays, AgingDays                                                          int
}

func (s *Scanner) RunOnce(ctx context.Context) error {
	if s.CheckBusinessLicense != nil {
		if err := s.CheckBusinessLicense(ctx); err != nil {
			return err
		}
	}
	if s.DB == nil {
		return errors.New("dunning scanner requires a database")
	}
	if s.BatchSize < 1 || s.BatchSize > 200 {
		s.BatchSize = 50
	}
	if err := s.closeSettled(ctx); err != nil {
		return err
	}
	if err := s.reopenSettled(ctx); err != nil {
		return err
	}
	if err := s.createInitialCases(ctx); err != nil {
		return err
	}
	return s.runDueCases(ctx)
}

func (s *Scanner) closeSettled(ctx context.Context) error {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE settlement_dunning_action da
JOIN settlement_dunning_case dc ON dc.id=da.dunning_case_id AND dc.tenant_id=da.tenant_id
JOIN settlement_receivable r ON r.id=dc.receivable_id AND r.tenant_id=dc.tenant_id
SET da.status='CANCELLED',da.version=da.version+1,da.updated_at=UTC_TIMESTAMP(3),da.result_detail='RECEIVABLE_SETTLED'
WHERE da.status IN ('PENDING','SENT') AND r.open_amount=0`); err != nil {
		return err
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE settlement_dunning_case dc
JOIN settlement_receivable r ON r.id=dc.receivable_id AND r.tenant_id=dc.tenant_id
SET dc.status='CLOSED',dc.closed_reason='RECEIVABLE_SETTLED',dc.closed_at=UTC_TIMESTAMP(3),dc.next_action_at=UTC_TIMESTAMP(3),dc.updated_at=UTC_TIMESTAMP(3)
WHERE dc.status='ACTIVE' AND r.open_amount=0`)
	return err
}

func (s *Scanner) reopenSettled(ctx context.Context) error {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	// 部分付款或冲销更正后，应收可能再次逾期。
	// 重新打开相同策略版本案件，不能被唯一键隐藏。
	_, err := s.DB.ExecContext(ctx, `UPDATE settlement_dunning_case dc
JOIN settlement_receivable r ON r.id=dc.receivable_id AND r.tenant_id=dc.tenant_id
JOIN settlement_dunning_policy p ON p.id=dc.dunning_policy_id AND p.tenant_id=dc.tenant_id AND p.version=dc.policy_version
SET dc.status='ACTIVE',dc.closed_reason='',dc.closed_at=NULL,dc.current_escalation_level=0,dc.last_action_at=NULL,dc.next_action_at=UTC_TIMESTAMP(3),dc.updated_at=UTC_TIMESTAMP(3)
WHERE dc.status='CLOSED' AND dc.closed_reason='RECEIVABLE_SETTLED' AND p.enabled=TRUE
  AND r.open_amount>0 AND r.due_date<CURDATE()
  AND DATEDIFF(CURDATE(),r.due_date) BETWEEN p.aging_from_days AND p.aging_to_days`)
	return err
}

func (s *Scanner) createInitialCases(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.tenant_id,r.id,r.receivable_no,cs.customer_name_snapshot,p.id,p.version,p.action_type,p.recipient_rule,p.channel,p.priority,p.repeat_interval_days,DATEDIFF(CURDATE(),r.due_date)
FROM settlement_receivable r
JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id
JOIN settlement_dunning_policy p ON p.id=(
  SELECT p2.id FROM settlement_dunning_policy p2
  WHERE p2.tenant_id=r.tenant_id AND p2.enabled=TRUE
    AND DATEDIFF(CURDATE(),r.due_date) BETWEEN p2.aging_from_days AND p2.aging_to_days
  ORDER BY p2.aging_from_days DESC,p2.id LIMIT 1
)
LEFT JOIN settlement_dunning_case dc ON dc.receivable_id=r.id AND dc.tenant_id=r.tenant_id
  AND dc.dunning_policy_id=p.id AND dc.policy_version=p.version
WHERE r.open_amount>0 AND r.due_date<CURDATE() AND dc.id IS NULL
ORDER BY r.due_date,r.id LIMIT ?`, s.BatchSize)
	if err != nil {
		return err
	}
	defer rows.Close()
	items := []candidate{}
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.TenantID, &c.ReceivableID, &c.Number, &c.Customer, &c.PolicyID, &c.PolicyVersion, &c.ActionType, &c.RecipientRule, &c.Channel, &c.Priority, &c.RepeatDays, &c.AgingDays); err != nil {
			return err
		}
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range items {
		recipient, err := recipientUser(c.RecipientRule)
		if err != nil {
			return err
		}
		eligible, err := s.eligible(ctx, recipient)
		if err != nil || !eligible {
			// 安全拒绝：目录不可用或目标已不再是催收员时，
			// 不分配新的责任。
			continue
		}
		frozen, err := s.frozen(ctx, c.TenantID, recipient)
		if err != nil || frozen {
			continue
		}
		if err := s.createInitial(ctx, c, recipient); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) runDueCases(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT dc.tenant_id,dc.id,dc.dunning_policy_id,dc.policy_version,dc.current_escalation_level,
r.id,r.receivable_no,cs.customer_name_snapshot,DATEDIFF(CURDATE(),r.due_date),
p.action_type,p.channel,p.priority,p.repeat_interval_days,
COALESCE((SELECT da.recipient_user_id FROM settlement_dunning_action da
  WHERE da.tenant_id=dc.tenant_id AND da.dunning_case_id=dc.id AND da.status IN ('PENDING','COMPLETED')
  ORDER BY da.created_at DESC,da.id DESC LIMIT 1),p.recipient_rule)
FROM settlement_dunning_case dc
JOIN settlement_receivable r ON r.id=dc.receivable_id AND r.tenant_id=dc.tenant_id
JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id
JOIN settlement_dunning_policy p ON p.id=dc.dunning_policy_id AND p.tenant_id=dc.tenant_id AND p.version=dc.policy_version AND p.enabled=TRUE
WHERE dc.status='ACTIVE' AND r.open_amount>0 AND dc.next_action_at<=UTC_TIMESTAMP(3)
ORDER BY dc.next_action_at,dc.id LIMIT ?`, s.BatchSize)
	if err != nil {
		return err
	}
	defer rows.Close()
	items := []dueCase{}
	for rows.Next() {
		var d dueCase
		if err := rows.Scan(&d.TenantID, &d.CaseID, &d.PolicyID, &d.PolicyVersion, &d.Level, &d.ReceivableID, &d.Number, &d.Customer, &d.AgingDays, &d.ActionType, &d.Channel, &d.Priority, &d.RepeatDays, &d.Recipient); err != nil {
			return err
		}
		items = append(items, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range items {
		recipient, err := storedRecipient(d.Recipient)
		if err != nil {
			return err
		}
		eligible, dirErr := s.eligible(ctx, recipient)
		frozen, freezeErr := s.frozen(ctx, d.TenantID, recipient)
		if dirErr != nil || freezeErr != nil || !eligible || frozen {
			if err := s.deferCase(ctx, d); err != nil {
				return err
			}
			continue
		}
		if err := s.createRepeat(ctx, d, recipient); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) createInitial(ctx context.Context, c candidate, recipient string) error {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	caseID, eventID := id(), id()
	repeat := c.RepeatDays
	if repeat <= 0 {
		repeat = 1
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT IGNORE INTO settlement_dunning_case(id,tenant_id,receivable_id,dunning_policy_id,policy_version,current_escalation_level,status,next_action_at,last_action_at,created_at,updated_at)
SELECT ?,r.tenant_id,r.id,?,?,1,'ACTIVE',DATE_ADD(UTC_TIMESTAMP(3),INTERVAL ? DAY),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3)
FROM settlement_receivable r WHERE r.id=? AND r.tenant_id=? AND r.open_amount>0 AND r.due_date<CURDATE()`, caseID, c.PolicyID, c.PolicyVersion, repeat, c.ReceivableID, c.TenantID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return tx.Commit()
	}
	if err := s.insertAction(ctx, tx, c.TenantID, caseID, c.ReceivableID, c.ActionType, recipient, c.Channel, c.Priority, eventID, c.Number, c.Customer, c.AgingDays); err != nil {
		return err
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Scanner) createRepeat(ctx context.Context, d dueCase, recipient string) error {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var nextActionAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT status,next_action_at FROM settlement_dunning_case WHERE tenant_id=? AND id=? FOR UPDATE`, d.TenantID, d.CaseID).Scan(&status, &nextActionAt); err != nil {
		return err
	}
	if status != "ACTIVE" || nextActionAt.After(s.now()) {
		return tx.Commit()
	}
	repeat := d.RepeatDays
	if repeat <= 0 {
		repeat = 1
	}
	eventID := id()
	if err := s.insertAction(ctx, tx, d.TenantID, d.CaseID, d.ReceivableID, d.ActionType, recipient, d.Channel, d.Priority, eventID, d.Number, d.Customer, d.AgingDays); err != nil {
		return err
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE settlement_dunning_case
SET current_escalation_level=LEAST(current_escalation_level+1,5),next_action_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL ? DAY),last_action_at=UTC_TIMESTAMP(3),updated_at=UTC_TIMESTAMP(3)
WHERE tenant_id=? AND id=? AND status='ACTIVE'`, repeat, d.TenantID, d.CaseID); err != nil {
		return err
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Scanner) insertAction(ctx context.Context, tx *sql.Tx, tenant, caseID, receivableID, actionType, recipient, channel, priority, eventID, number, customer string, aging int) error {
	// 目录及行锁等待可能跨越签名到期时间。检查每次
	// 写入，暂停时由调用方回滚整个事务。
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	title := "应收逾期提醒"
	content := fmt.Sprintf("%s的应收单%s已逾期%d天，请及时跟进并完成催收动作。", customer, number, aging)
	result, err := tx.ExecContext(ctx, `INSERT INTO settlement_dunning_action(id,tenant_id,dunning_case_id,action_type,recipient_user_id,channel,priority,event_id,status,version,sent_at,updated_at,created_at)
VALUES(?,?,?,?,?,?,?,?, 'PENDING',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id(), tenant, caseID, actionType, recipient, channel, priority, eventID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return err
		}
		return errors.New("dunning action insert affected no row")
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settlement_local_notification(id,tenant_id,recipient_user_id,event_id,notification_scope,priority,title,content,target_url,reference_type,reference_id,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3))`, id(), tenant, recipient, eventID, "LOCAL", priority, title, content, "/settlement/dunning", "RECEIVABLE", receivableID); err != nil {
		return err
	}
	if channel == "PLATFORM" || priority == "HIGH" || priority == "CRITICAL" {
		if err := s.checkBusinessLicense(ctx); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"event_id": eventID, "event_type": "SETTLEMENT_RECEIVABLE_OVERDUE", "notification_scope": "CROSS_SYSTEM", "priority": priority, "title": title, "content": content, "target_url": "/settlement/dunning", "reference_type": "RECEIVABLE", "reference_id": receivableID, "idempotency_key": eventID, "recipient_user_ids": []string{recipient}, "occurred_at": s.now().UTC().Format(time.RFC3339Nano)})
		if _, err := tx.ExecContext(ctx, `INSERT INTO settlement_outbox_event(id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,status,available_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id(), tenant, eventID, "PLATFORM_NOTIFICATION", "SETTLEMENT_RECEIVABLE_OVERDUE", "receivable", caseID, payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) deferCase(ctx context.Context, d dueCase) error {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE settlement_dunning_case SET next_action_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 1 HOUR),updated_at=UTC_TIMESTAMP(3)
WHERE tenant_id=? AND id=? AND status='ACTIVE'`, d.TenantID, d.CaseID)
	return err
}

func (s *Scanner) checkBusinessLicense(ctx context.Context) error {
	if s.CheckBusinessLicense != nil {
		return s.CheckBusinessLicense(ctx)
	}
	return nil
}

func (s *Scanner) eligible(ctx context.Context, userID string) (bool, error) {
	if s.Directory == nil {
		return true, nil
	}
	return s.Directory.ContainsCollector(ctx, userID)
}

func (s *Scanner) frozen(ctx context.Context, tenantID, userID string) (bool, error) {
	var frozen bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM settlement_handover_freeze WHERE tenant_id=? AND user_id=? AND status='FROZEN')`, tenantID, userID).Scan(&frozen)
	return frozen, err
}

func (s *Scanner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func storedRecipient(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "USER:") {
		return recipientUser(value)
	}
	if value == "" {
		return "", fmt.Errorf("stored recipient is empty")
	}
	return value, nil
}

func recipientUser(rule string) (string, error) {
	recipient := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rule), "USER:"))
	if recipient == "" || !strings.HasPrefix(strings.TrimSpace(rule), "USER:") {
		return "", fmt.Errorf("unsupported recipient rule %q", rule)
	}
	return recipient, nil
}

func id() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}
