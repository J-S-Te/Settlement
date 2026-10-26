package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

func outbox(ctx context.Context, tx *sql.Tx, tenant, destination, eventType, aggregateType, aggregateID string, payload any) error {
	id := newID()
	if destination == "TAX_INVOICE_COMMAND" {
		if source, ok := payload.(map[string]any); ok {
			envelope := make(map[string]any, len(source)+5)
			for key, value := range source {
				envelope[key] = value
			}
			envelope["event_id"] = id
			envelope["event_type"] = eventType
			envelope["schema_version"] = 1
			envelope["tenant_id"] = tenant
			envelope["occurred_at"] = time.Now().UTC()
			payload = envelope
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_outbox_event(id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,status,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?, 'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, id, destination, eventType, aggregateType, aggregateID, body)
	if err != nil {
		return err
	}
	if destination == "PLATFORM_AUDIT" {
		// AUD-IP-2026：审计 detail 必须携带三类来源地址，平台据此区分
		// 登录地址、操作地址与投递地址，而不是拿投递连接地址冒充用户地址。
		if value, ok := payload.(map[string]any); ok {
			enrichAuditIP(value, ctx)
		}
		actor := "SYSTEM"
		if value, ok := payload.(map[string]any); ok {
			if v, ok := value["actor_id"].(string); ok && v != "" {
				actor = v
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_audit_event(id,tenant_id,actor_id,action,resource_type,resource_id,result,reason_code,request_id,correlation_id,detail_json,occurred_at) VALUES(?,?,?,?,?,?,'SUCCESS','',?,?,?,UTC_TIMESTAMP(3))`, newID(), tenant, actor, eventType, aggregateType, aggregateID, id, id, body)
	}
	return err
}

// normalizeInvoiceItems 是发票明细运算的服务端唯一权威
// 来源。数量、单价和税率最多接受六位小数；
// 金额分量四舍五入到两位，必须与提交值
// 精确相等。任何尾差均拒绝，不自动吸收。
