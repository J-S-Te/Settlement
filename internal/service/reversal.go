package service

import (
	"context"
	"fmt"
	"math/big"
)

// ReverseAllocation 通过独立审批冲销普通核销，历史恢复缺少适配器时明确阻塞。
// 参数：ctx为上下文，p为主体，key为幂等键，allocationID为原核销，in为原因及金额。
// 返回值：冲销ID或校验、授权及持久化错误。
func (s *Service) ReverseAllocation(ctx context.Context, p Principal, key, allocationID string, in ReversalInput) (string, error) {
	if textTooLong(in.ReasonCode, 64) || textTooLong(in.ReasonDetail, 500) {
		return "", fmt.Errorf("%w: reversal reason exceeds the length limit", ErrInvalid)
	}
	value, err := amount(in.Amount)
	if err != nil {
		return "", err
	}
	if _, err = required(key, "Idempotency-Key"); err != nil {
		return "", err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var receiptID, receivableID, currency, confirmedBy, allocated, reversed string
	err = tx.QueryRowContext(ctx, `SELECT a.receipt_id,a.receivable_id,a.currency,a.confirmed_by,a.allocated_amount,COALESCE((SELECT SUM(r.reversed_amount) FROM settlement_receipt_allocation_reversal r WHERE r.original_allocation_id=a.id),0) FROM settlement_receipt_allocation a WHERE a.id=? AND a.tenant_id=? AND a.status IN('CONFIRMED','PARTIALLY_REVERSED') FOR UPDATE`, allocationID, p.TenantID).Scan(&receiptID, &receivableID, &currency, &confirmedBy, &allocated, &reversed)
	if err != nil {
		return "", ErrConflict
	}
	if err = rejectImportedReceiptMutation(ctx, tx, p.TenantID, receiptID); err != nil {
		return "", err
	}
	if confirmedBy == p.UserID {
		return "", fmt.Errorf("%w: allocation confirmer cannot approve reversal", ErrInvalid)
	}
	remaining := new(big.Rat).Sub(add(allocated), add(reversed))
	if decimalGreater(value, remaining.FloatString(2)) {
		return "", ErrConflict
	}
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receipt_allocation_reversal(id,tenant_id,reversal_no,original_allocation_id,reversed_amount,reason_code,reason_detail,approved_by,reversed_at,idempotency_key) VALUES(?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),?)`, id, p.TenantID, "RV-"+id[:12], allocationID, value, in.ReasonCode, in.ReasonDetail, p.UserID, key)
	if err != nil {
		if mysqlDuplicate(err) {
			var existing string
			if e := tx.QueryRowContext(ctx, `SELECT id FROM settlement_receipt_allocation_reversal WHERE tenant_id=? AND idempotency_key=?`, p.TenantID, key).Scan(&existing); e == nil {
				return existing, nil
			}
		}
		return "", err
	}
	full := remaining.Cmp(add(value)) == 0
	status := "PARTIALLY_REVERSED"
	if full {
		status = "REVERSED"
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receipt_allocation SET status=? WHERE id=? AND tenant_id=?`, status, allocationID, p.TenantID)
	if err != nil {
		return "", err
	}
	var receiptUnallocated, receiptAmount, receivableReceived, receivableOpen string
	if err = tx.QueryRowContext(ctx, `SELECT unallocated_amount,amount FROM settlement_receipt WHERE id=? AND tenant_id=? FOR UPDATE`, receiptID, p.TenantID).Scan(&receiptUnallocated, &receiptAmount); err != nil {
		return "", ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT received_allocated_amount,open_amount FROM settlement_receivable WHERE id=? AND tenant_id=? FOR UPDATE`, receivableID, p.TenantID).Scan(&receivableReceived, &receivableOpen); err != nil {
		return "", ErrConflict
	}
	// 只有离开 SETTLED 的转换才有结清事实可撤销。
	// 对已部分结清期间的冲销不得再次补偿
	// 较早轮次。
	settledBeforeReversal := add(receivableOpen).Sign() == 0
	receiptAfter := new(big.Rat).Add(add(receiptUnallocated), add(value))
	receiptStatus := "PARTIALLY_ALLOCATED"
	if receiptAfter.Cmp(add(receiptAmount)) == 0 {
		receiptStatus = "AVAILABLE"
	}
	receivableReceivedAfter := new(big.Rat).Sub(add(receivableReceived), add(value))
	if receivableReceivedAfter.Sign() < 0 {
		return "", ErrConflict
	}
	receivableOpenAfter := new(big.Rat).Add(add(receivableOpen), add(value))
	receivableStatus := "PARTIALLY_SETTLED"
	if receivableReceivedAfter.Sign() == 0 {
		receivableStatus = "UNPAID"
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receipt SET unallocated_amount=?,status=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, receiptAfter.FloatString(2), receiptStatus, receiptID, p.TenantID)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET received_allocated_amount=?,open_amount=?,collection_status=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, receivableReceivedAfter.FloatString(2), receivableOpenAfter.FloatString(2), receivableStatus, receivableID, p.TenantID)
	if err != nil {
		return "", err
	}
	var reversalCreditEventID string
	if settledBeforeReversal {
		reversalCreditEventID, err = enqueueCreditReversal(ctx, tx, p.TenantID, allocationID, receivableID)
		if err != nil {
			return "", err
		}
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_ALLOCATION_REVERSED", "receipt_allocation", allocationID, map[string]any{"actor_id": p.UserID, "reversal_id": id, "reason_code": in.ReasonCode, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if reversalCreditEventID != "" && s.CreditPublisher != nil {
		_ = s.RetryCreditSync(ctx, p.TenantID, reversalCreditEventID)
	}
	return id, nil
}

type DunningPolicyInput struct {
	Name               string `json:"name"`
	AgingFromDays      int    `json:"aging_from_days"`
	AgingToDays        int    `json:"aging_to_days"`
	ActionType         string `json:"action_type"`
	RecipientRule      string `json:"recipient_rule"`
	Channel            string `json:"channel"`
	RepeatIntervalDays int    `json:"repeat_interval_days"`
	Priority           string `json:"priority"`
}

const DunningActionNoteLimit = 500

// CompleteDunningAction 记录催收员确认。动作仅在该命令成功前
// 保持 PENDING；扫描器在案件关闭时取消待处理
// 动作。版本机制使重试和并发标签页安全。
