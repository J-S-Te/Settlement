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
	// AUD-IP-2026：来源地址必须在序列化前富化，审计 detail 与 outbox payload
	// 共用同一份富化后的 body；否则审计缺少地址，平台只能拿投递连接地址冒充。
	// 平台据此区分登录地址、操作地址与投递地址。
	var auditDetail map[string]any
	if destination == "PLATFORM_AUDIT" {
		if value, ok := payload.(map[string]any); ok {
			enrichAuditIP(value, ctx)
			auditDetail = value
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
		actor := "SYSTEM"
		// 本地审计 result 与平台侧同源：取 payload 的 result，缺省才 SUCCESS；
		// 硬编码 SUCCESS 会把失败操作伪装成成功。
		result := "SUCCESS"
		if auditDetail != nil {
			if v, ok := auditDetail["actor_id"].(string); ok && v != "" {
				actor = v
			}
			if v, ok := auditDetail["result"].(string); ok && v != "" {
				result = v
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_audit_event(id,tenant_id,actor_id,action,resource_type,resource_id,result,reason_code,request_id,correlation_id,detail_json,occurred_at) VALUES(?,?,?,?,?,?,?,'',?,?,?,UTC_TIMESTAMP(3))`, newID(), tenant, actor, eventType, aggregateType, aggregateID, result, id, id, body)
	}
	return err
}

// normalizeInvoiceItems 是发票明细运算的服务端唯一权威
// 来源。数量、单价和税率最多接受六位小数；
// 金额分量四舍五入到两位，必须与提交值
// 精确相等。任何尾差均拒绝，不自动吸收。
