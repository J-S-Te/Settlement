package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// TaxCommandDeadLetter 是税控命令死信的只读运维视图。
// 它复用 settlement_outbox_event 的死信状态，不新增第二套
// 死信事实；申请、尝试和案卷状态来自各自的权威表。
type TaxCommandDeadLetter struct {
	ID               string `json:"id"`
	EventID          string `json:"event_id"`
	EventType        string `json:"event_type"`
	OperationType    string `json:"operation_type"`
	AggregateID      string `json:"aggregate_id"`
	AttemptID        string `json:"attempt_id"`
	RequestID        string `json:"request_id"`
	AttemptStatus    string `json:"attempt_status"`
	RequestStatus    string `json:"request_status"`
	AttemptCount     int    `json:"attempt_count"`
	LastErrorCode    string `json:"last_error_code"`
	LastErrorSummary string `json:"last_error_summary"`
	DeadLetteredAt   string `json:"dead_lettered_at"`
	CreatedAt        string `json:"created_at"`
}

// ListTaxCommandDeadLetters 用途：按租户列出税控命令死信，可按开票申请或命令过滤。
// 参数：ctx为上下文，p为可信主体，requestID为开票/红冲申请，commandID为命令事件。
// 返回值：按死信时间倒序的有界列表；无权或查询失败返回错误。
func (s *Service) ListTaxCommandDeadLetters(ctx context.Context, p Principal, requestID, commandID string) ([]TaxCommandDeadLetter, error) {
	if !p.Allows("settlement.invoice.issue") {
		return nil, ErrForbidden
	}
	if strings.TrimSpace(p.TenantID) == "" {
		return nil, ErrForbidden
	}
	requestID = strings.TrimSpace(requestID)
	commandID = strings.TrimSpace(commandID)
	if len(requestID) > 64 || len(commandID) > 64 {
		return nil, fmt.Errorf("%w: dead letter filter is too long", ErrInvalid)
	}
	query := `SELECT oe.id,oe.event_id,oe.event_type,oe.aggregate_id,oe.attempt_count,oe.last_error_code,oe.last_error_summary,
COALESCE(DATE_FORMAT(oe.dead_lettered_at,'%Y-%m-%dT%H:%i:%s.%fZ'),''),DATE_FORMAT(oe.created_at,'%Y-%m-%dT%H:%i:%s.%fZ'),
COALESCE(ia.id,''),COALESCE(ia.invoice_request_id,''),COALESCE(ia.status,''),
COALESCE(ra.id,''),COALESCE(ra.red_flush_request_id,''),COALESCE(ra.status,''),
COALESCE(ir.status,''),COALESCE(rf.status,'')
FROM settlement_outbox_event oe
LEFT JOIN settlement_invoice_issue_attempt ia ON ia.tenant_id=oe.tenant_id AND ia.command_event_id=oe.event_id
LEFT JOIN settlement_invoice_request ir ON ir.id=ia.invoice_request_id AND ir.tenant_id=ia.tenant_id
LEFT JOIN settlement_invoice_red_flush_attempt ra ON ra.tenant_id=oe.tenant_id AND ra.command_event_id=oe.event_id
LEFT JOIN settlement_invoice_red_flush_request rf ON rf.id=ra.red_flush_request_id AND rf.tenant_id=ra.tenant_id
WHERE oe.tenant_id=? AND oe.destination='TAX_INVOICE_COMMAND' AND oe.status='DEAD_LETTER'`
	args := []any{p.TenantID}
	if commandID != "" {
		query += ` AND oe.id=?`
		args = append(args, commandID)
	}
	if requestID != "" {
		query += ` AND (ia.invoice_request_id=? OR ra.red_flush_request_id=?)`
		args = append(args, requestID, requestID)
	}
	query += ` ORDER BY oe.dead_lettered_at DESC,oe.id DESC LIMIT 200`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaxCommandDeadLetter{}
	for rows.Next() {
		var item TaxCommandDeadLetter
		var issueAttemptID, issueRequestID, issueAttemptStatus string
		var redAttemptID, redRequestID, redAttemptStatus string
		var issueRequestStatus, redRequestStatus string
		if err = rows.Scan(&item.ID, &item.EventID, &item.EventType, &item.AggregateID, &item.AttemptCount, &item.LastErrorCode, &item.LastErrorSummary, &item.DeadLetteredAt, &item.CreatedAt,
			&issueAttemptID, &issueRequestID, &issueAttemptStatus, &redAttemptID, &redRequestID, &redAttemptStatus, &issueRequestStatus, &redRequestStatus); err != nil {
			return nil, err
		}
		item.OperationType = taxCommandOperation(item.EventType)
		if redAttemptID != "" || item.OperationType == "RED_FLUSH" {
			item.AttemptID, item.RequestID, item.AttemptStatus, item.RequestStatus = redAttemptID, redRequestID, redAttemptStatus, redRequestStatus
		} else {
			item.AttemptID, item.RequestID, item.AttemptStatus, item.RequestStatus = issueAttemptID, issueRequestID, issueAttemptStatus, issueRequestStatus
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// taxCommandOperation 把税控命令事件类型收敛为稳定操作类型。
// 参数：eventType为outbox事件类型。
// 返回值：INVOICE_ISSUE、RED_FLUSH或空字符串（未知命令）。
func taxCommandOperation(eventType string) string {
	switch strings.TrimSpace(eventType) {
	case "SETTLEMENT_INVOICE_ISSUE_REQUESTED":
		return "INVOICE_ISSUE"
	case "SETTLEMENT_INVOICE_RED_FLUSH_REQUESTED":
		return "RED_FLUSH"
	default:
		return ""
	}
}

// TaxCommandDeadLetterRetryInput 是人工重试死信所需的原因说明。
type TaxCommandDeadLetterRetryInput struct {
	Reason string `json:"reason"`
}

// RetryTaxCommandDeadLetter 用途：在财务显式指令下把一条税控命令死信重新投递。
// 不创建新命令、不改变申请状态、不释放额度预留；失败仍回到死信，
// 由人工再次决定。相同幂等键重放返回首次结果而不重复改状态或审计。
// 参数：ctx为上下文，p为可信主体，key为幂等键，id为outbox命令，in为重试原因。
// 返回值：权限、状态机、冲突或存储错误；成功时已提交。
func (s *Service) RetryTaxCommandDeadLetter(ctx context.Context, p Principal, key, id string, in TaxCommandDeadLetterRetryInput) error {
	if !p.Allows("settlement.invoice.issue") || strings.TrimSpace(p.TenantID) == "" || strings.TrimSpace(p.UserID) == "" {
		return ErrForbidden
	}
	id = strings.TrimSpace(id)
	key = strings.TrimSpace(key)
	reason := strings.TrimSpace(in.Reason)
	if id == "" || key == "" || len(key) > 128 || len([]rune(reason)) < 5 || len([]rune(reason)) > 1000 {
		return fmt.Errorf("%w: 重试原因应为 5 到 1000 字，且必须提供幂等键", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, replay, err := claimIdempotency(ctx, tx, p.TenantID, "RETRY_TAX_COMMAND_DEAD_LETTER", key, id, map[string]any{"actor_id": p.UserID, "id": id, "reason": reason})
	if err != nil {
		return err
	}
	if replay {
		return tx.Commit()
	}
	var destination, status, eventType, eventID, aggregateType, lastCode, lastSummary string
	var payload []byte
	err = tx.QueryRowContext(ctx, `SELECT destination,status,event_type,event_id,aggregate_type,payload_json,last_error_code,last_error_summary FROM settlement_outbox_event WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, id).Scan(&destination, &status, &eventType, &eventID, &aggregateType, &payload, &lastCode, &lastSummary)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if destination != "TAX_INVOICE_COMMAND" || status != "DEAD_LETTER" {
		return fmt.Errorf("%w: 只能人工重试税控命令死信", ErrConflict)
	}
	var command struct {
		AttemptID         string `json:"attempt_id"`
		RedFlushAttemptID string `json:"red_flush_attempt_id"`
	}
	if json.Unmarshal(payload, &command) != nil {
		return fmt.Errorf("%w: 税控命令载荷不可解析", ErrConflict)
	}
	operation := taxCommandOperation(eventType)
	if operation == "" {
		return fmt.Errorf("%w: 未知税控命令类型", ErrConflict)
	}
	switch operation {
	case "INVOICE_ISSUE":
		if aggregateType != "invoice_request" || strings.TrimSpace(command.AttemptID) == "" {
			return fmt.Errorf("%w: 开票命令缺少申请或尝试标识", ErrConflict)
		}
		if err = s.requireRetryableIssueCommandTx(ctx, tx, p, eventID, command.AttemptID); err != nil {
			return err
		}
	case "RED_FLUSH":
		if aggregateType != "tax_invoice" || strings.TrimSpace(command.RedFlushAttemptID) == "" {
			return fmt.Errorf("%w: 红冲命令缺少尝试标识", ErrConflict)
		}
		if err = s.requireRetryableRedFlushCommandTx(ctx, tx, p, eventID, command.RedFlushAttemptID); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE settlement_outbox_event SET status='PENDING',attempt_count=0,available_at=UTC_TIMESTAMP(3),locked_by=NULL,locked_until=NULL,last_error_code='',last_error_summary='',dead_lettered_at=NULL,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND destination='TAX_INVOICE_COMMAND' AND status='DEAD_LETTER'`, p.TenantID, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrConflict
	}
	attemptID := command.AttemptID
	if operation == "RED_FLUSH" {
		attemptID = command.RedFlushAttemptID
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_TAX_COMMAND_DEAD_LETTER_RETRIED", "outbox_event", id, map[string]any{"actor_id": p.UserID, "operation_type": operation, "attempt_id": attemptID, "command_event_id": eventID, "reason": reason, "previous_error_code": lastCode, "previous_error_summary": lastSummary, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return err
	}
	return tx.Commit()
}

// requireRetryableIssueCommandTx 用途：在重投开票命令前核对尝试、申请和命令身份。
// 只有"命令从未被税控接受、申请仍在开票中、命令仍是该尝试最新命令"才允许重试。
// 参数：ctx为上下文，tx为当前事务，p为主体，eventID为命令事件，attemptID为尝试。
// 返回值：可重试时无错误；状态不明、已被取代或范围外返回冲突/不存在。
func (s *Service) requireRetryableIssueCommandTx(ctx context.Context, tx *sql.Tx, p Principal, eventID, attemptID string) error {
	var attemptStatus, requestID, commandEventID, requestStatus string
	var attemptNo int
	var neverAccepted bool
	err := tx.QueryRowContext(ctx, `SELECT ia.status,(ia.accepted_at IS NULL),ia.invoice_request_id,COALESCE(ia.command_event_id,''),ia.attempt_no,ir.status FROM settlement_invoice_issue_attempt ia JOIN settlement_invoice_request ir ON ir.id=ia.invoice_request_id AND ir.tenant_id=ia.tenant_id WHERE ia.id=? AND ia.tenant_id=? FOR UPDATE`, attemptID, p.TenantID).Scan(&attemptStatus, &neverAccepted, &requestID, &commandEventID, &attemptNo, &requestStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 税控尝试不存在", ErrConflict)
	}
	if err != nil {
		return err
	}
	if attemptStatus != "PENDING" || !neverAccepted || requestStatus != "ISSUE_PENDING" {
		return fmt.Errorf("%w: 开票命令已不在可重试状态", ErrConflict)
	}
	if commandEventID != eventID {
		return fmt.Errorf("%w: 死信命令已被新的税控命令取代", ErrConflict)
	}
	// 申请失败复核后可能已产生更新的尝试；旧尝试即使仍是 PENDING，
	// 也不能再向税控重复投递同一申请。
	var latest bool
	if err = tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM settlement_invoice_issue_attempt newer WHERE newer.tenant_id=? AND newer.invoice_request_id=? AND newer.attempt_no>?)`, p.TenantID, requestID, attemptNo).Scan(&latest); err != nil {
		return err
	}
	if !latest {
		return fmt.Errorf("%w: 死信命令已被更新的开票尝试取代", ErrConflict)
	}
	return s.requireInvoiceRequestScopeTx(ctx, tx, p, "settlement.invoice.issue", requestID)
}

// requireRetryableRedFlushCommandTx 用途：在重投红冲命令前核对尝试、申请和原票状态。
// 红冲死信不涉及额度预留，但仍不得重投已被结果收敛或已被取代的命令。
// 参数：ctx为上下文，tx为当前事务，p为主体，eventID为命令事件，attemptID为红冲尝试。
// 返回值：可重试时无错误；状态、原票或范围不合法返回冲突/不存在。
func (s *Service) requireRetryableRedFlushCommandTx(ctx context.Context, tx *sql.Tx, p Principal, eventID, attemptID string) error {
	var attemptStatus, requestID, commandEventID, requestStatus, originalInvoiceID, originalStatus string
	var attemptNo int
	var neverAccepted bool
	err := tx.QueryRowContext(ctx, `SELECT ra.status,(ra.accepted_at IS NULL),ra.red_flush_request_id,COALESCE(ra.command_event_id,''),ra.attempt_no,rf.status,ra.original_invoice_id,ti.status FROM settlement_invoice_red_flush_attempt ra JOIN settlement_invoice_red_flush_request rf ON rf.id=ra.red_flush_request_id AND rf.tenant_id=ra.tenant_id JOIN settlement_tax_invoice ti ON ti.id=ra.original_invoice_id AND ti.tenant_id=ra.tenant_id WHERE ra.id=? AND ra.tenant_id=? FOR UPDATE`, attemptID, p.TenantID).Scan(&attemptStatus, &neverAccepted, &requestID, &commandEventID, &attemptNo, &requestStatus, &originalInvoiceID, &originalStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 红冲尝试不存在", ErrConflict)
	}
	if err != nil {
		return err
	}
	if attemptStatus != "PENDING" || !neverAccepted || requestStatus != "ISSUE_PENDING" || originalStatus != "ISSUED" {
		return fmt.Errorf("%w: 红冲命令已不在可重试状态", ErrConflict)
	}
	if commandEventID != eventID {
		return fmt.Errorf("%w: 死信命令已被新的红冲命令取代", ErrConflict)
	}
	var latest bool
	if err = tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM settlement_invoice_red_flush_attempt newer WHERE newer.tenant_id=? AND newer.red_flush_request_id=? AND newer.attempt_no>?)`, p.TenantID, requestID, attemptNo).Scan(&latest); err != nil {
		return err
	}
	if !latest {
		return fmt.Errorf("%w: 死信命令已被更新的红冲尝试取代", ErrConflict)
	}
	return s.requireTaxInvoiceScopeTx(ctx, tx, p, "settlement.invoice.issue", originalInvoiceID)
}
