package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"time"
)

type approvalResultNotice struct {
	recipient, eventType, subject, referenceType, referenceID, targetURL string
	approved                                                             bool
}

// enqueueApprovalResult 将终审结果投递给原申请人。
// 参数：ctx 为审批上下文，tx 为同一审批事务，tenant 为归属租户，notice 为审批结果与业务坐标。
// 返回值：写入平台通知发件箱的错误，成功时为 nil。
func enqueueApprovalResult(ctx context.Context, tx *sql.Tx, tenant string, notice approvalResultNotice) error {
	id := newID()
	// 发件箱事件 ID 是 CHAR(32)，且平台只接受字母开头的事件 ID。
	eventID := "N" + id[1:]
	outcome := "已驳回"
	if notice.approved {
		outcome = "已通过"
	}
	title := notice.subject + outcome
	payload, err := json.Marshal(map[string]any{
		"event_id": eventID, "event_type": notice.eventType, "idempotency_key": eventID,
		"recipient_user_ids": []string{notice.recipient}, "reference_type": notice.referenceType,
		"reference_id": notice.referenceID, "notification_scope": "CROSS_SYSTEM", "priority": "NORMAL",
		"title": title, "content": title + "，请查看相关业务记录。", "target_url": notice.targetURL,
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_outbox_event
(id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,
 status,available_at,created_at,updated_at)
VALUES(?,?,?,'PLATFORM_NOTIFICATION',?,?,?,?,'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		id, tenant, eventID, notice.eventType, notice.referenceType, notice.referenceID, payload)
	return err
}

// enqueueInvoiceApprovalOutcome 写入开票申请的终审通知。
// 参数：ctx、tx 为审批事务上下文，tenant、requestID、applicant 标识申请及申请人，approved 为终审决定。
// 返回值：站内通知持久化错误，成功时为 nil。
func enqueueInvoiceApprovalOutcome(ctx context.Context, tx *sql.Tx, tenant, requestID, applicant string, approved bool) error {
	eventType := "SETTLEMENT_INVOICE_REJECTED"
	if approved {
		eventType = "SETTLEMENT_INVOICE_APPROVED"
	}
	return enqueueApprovalResult(ctx, tx, tenant, approvalResultNotice{
		recipient: applicant, eventType: eventType, subject: "开票申请", approved: approved,
		referenceType: "invoice_request", referenceID: requestID, targetURL: invoiceResultURL(requestID),
	})
}

// invoiceResultURL 构造开票审批结果对应的业务详情地址。
// 参数：requestID 为已持久化的开票申请。
// 返回值：带申请筛选条件的站内地址。
func invoiceResultURL(requestID string) string {
	return "/settlement/invoices?" + url.Values{"request_id": {requestID}}.Encode()
}
