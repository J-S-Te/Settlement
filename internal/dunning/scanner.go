package dunning

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Scanner struct {
	DB        *sql.DB
	BatchSize int
}
type candidate struct {
	TenantID, ReceivableID, Number, Customer, PolicyID, ActionType, RecipientRule, Channel, Priority string
	PolicyVersion, RepeatDays, AgingDays                                                             int
}

func (s *Scanner) RunOnce(ctx context.Context) error {
	if s.BatchSize < 1 || s.BatchSize > 200 {
		s.BatchSize = 50
	}
	_, _ = s.DB.ExecContext(ctx, `UPDATE settlement_dunning_case dc JOIN settlement_receivable r ON r.id=dc.receivable_id AND r.tenant_id=dc.tenant_id SET dc.status='CLOSED',dc.closed_reason='RECEIVABLE_SETTLED',dc.updated_at=UTC_TIMESTAMP(3) WHERE dc.status='ACTIVE' AND r.open_amount=0`)
	rows, err := s.DB.QueryContext(ctx, `SELECT r.tenant_id,r.id,r.receivable_no,cs.customer_name_snapshot,p.id,p.version,p.action_type,p.recipient_rule,p.channel,p.priority,p.repeat_interval_days,DATEDIFF(CURDATE(),r.due_date) FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id JOIN settlement_dunning_policy p ON p.tenant_id=r.tenant_id AND p.enabled=TRUE AND DATEDIFF(CURDATE(),r.due_date) BETWEEN p.aging_from_days AND p.aging_to_days LEFT JOIN settlement_dunning_case dc ON dc.receivable_id=r.id AND dc.tenant_id=r.tenant_id AND dc.dunning_policy_id=p.id AND dc.policy_version=p.version WHERE r.open_amount>0 AND r.due_date<CURDATE() AND dc.id IS NULL ORDER BY r.due_date LIMIT ?`, s.BatchSize)
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
		if err := s.create(ctx, c); err != nil {
			return err
		}
	}
	return nil
}
func (s *Scanner) create(ctx context.Context, c candidate) error {
	recipient := strings.TrimPrefix(c.RecipientRule, "USER:")
	if recipient == c.RecipientRule || strings.TrimSpace(recipient) == "" {
		return fmt.Errorf("unsupported recipient rule")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	caseID, eventID := id(), id()
	result, err := tx.ExecContext(ctx, `INSERT IGNORE INTO settlement_dunning_case(id,tenant_id,receivable_id,dunning_policy_id,policy_version,current_escalation_level,status,next_action_at,created_at,updated_at) SELECT ?,tenant_id,id,?,?,1,'ACTIVE',DATE_ADD(UTC_TIMESTAMP(3),INTERVAL ? DAY),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3) FROM settlement_receivable WHERE id=? AND tenant_id=? AND open_amount>0`, caseID, c.PolicyID, c.PolicyVersion, c.RepeatDays, c.ReceivableID, c.TenantID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return tx.Commit()
	}
	var tenant string
	if err = tx.QueryRowContext(ctx, `SELECT tenant_id FROM settlement_receivable WHERE id=? AND tenant_id=?`, c.ReceivableID, c.TenantID).Scan(&tenant); err != nil {
		return err
	}
	title := "应收逾期提醒"
	content := fmt.Sprintf("%s的应收单%s已逾期%d天，请及时处理。", c.Customer, c.Number, c.AgingDays)
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_dunning_action(id,tenant_id,dunning_case_id,action_type,recipient_user_id,channel,priority,event_id,status,created_at) VALUES(?,?,?,?,?,?,?,?, 'PENDING',UTC_TIMESTAMP(3))`, id(), tenant, caseID, c.ActionType, recipient, c.Channel, c.Priority, eventID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_local_notification(id,tenant_id,recipient_user_id,event_id,notification_scope,priority,title,content,target_url,reference_type,reference_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3))`, id(), tenant, recipient, eventID, "LOCAL", c.Priority, title, content, "/settlement/dunning", "RECEIVABLE", c.ReceivableID)
	if err != nil {
		return err
	}
	if c.Channel == "PLATFORM" || c.Priority == "HIGH" || c.Priority == "CRITICAL" {
		payload, _ := json.Marshal(map[string]any{"event_id": eventID, "event_type": "SETTLEMENT_RECEIVABLE_OVERDUE", "notification_scope": "CROSS_SYSTEM", "priority": c.Priority, "title": title, "content": content, "target_url": "/settlement/dunning", "reference_type": "RECEIVABLE", "reference_id": c.ReceivableID, "idempotency_key": eventID, "recipient_user_ids": []string{recipient}, "occurred_at": time.Now().UTC().Format(time.RFC3339Nano)})
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_outbox_event(id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,status,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id(), tenant, eventID, "PLATFORM_NOTIFICATION", "SETTLEMENT_RECEIVABLE_OVERDUE", "receivable", c.ReceivableID, payload)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func id() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}
