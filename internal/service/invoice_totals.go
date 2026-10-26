package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
)

func incrementInvoiced(ctx context.Context, tx *sql.Tx, tenant, receivableID, value string) error {
	var current, original string
	if err := tx.QueryRowContext(ctx, `SELECT invoiced_amount,original_amount FROM settlement_receivable WHERE id=? AND tenant_id=? FOR UPDATE`, receivableID, tenant).Scan(&current, &original); err != nil {
		return err
	}
	next := new(big.Rat).Add(add(current), add(value))
	status := "PARTIALLY_INVOICED"
	if next.Cmp(add(original)) >= 0 {
		status = "FULLY_INVOICED"
	}
	_, err := tx.ExecContext(ctx, `UPDATE settlement_receivable SET invoiced_amount=?,invoice_status=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, next.FloatString(2), status, receivableID, tenant)
	return err
}

// decrementInvoiced 对红冲或冲销执行与 incrementInvoiced 对应的反向操作。
// 既有行为将下限限制为零，状态依据限制后的值。
func decrementInvoiced(ctx context.Context, tx *sql.Tx, tenant, receivableID, value string) error {
	var current, original string
	if err := tx.QueryRowContext(ctx, `SELECT invoiced_amount,original_amount FROM settlement_receivable WHERE id=? AND tenant_id=? FOR UPDATE`, receivableID, tenant).Scan(&current, &original); err != nil {
		return err
	}
	next := new(big.Rat).Sub(add(current), add(value))
	if next.Sign() < 0 {
		next = new(big.Rat)
	}
	status := "PARTIALLY_INVOICED"
	if next.Sign() == 0 {
		status = "NOT_INVOICED"
	} else if next.Cmp(add(original)) >= 0 {
		status = "FULLY_INVOICED"
	}
	_, err := tx.ExecContext(ctx, `UPDATE settlement_receivable SET invoiced_amount=?,invoice_status=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, next.FloatString(2), status, receivableID, tenant)
	return err
}

// RequestMeta 保存本次请求可写入审计的来源信息（AUD-IP-2026）。
type RequestMeta struct {
	LoginIP, ClientIP, IPSource string
}

type requestMetaKey struct{}

// WithRequestMeta 由 HTTP 鉴权层在请求进入业务前写入。
func enqueueCreditReversal(ctx context.Context, tx *sql.Tx, tenant, allocationID, receivableID string) (string, error) {
	var originalEventID, eventType, status, settledAllocationID string
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT completed.event_id,completed.event_type,completed.status,completed.allocation_id,completed.payload_json
		FROM settlement_credit_sync completed
		WHERE completed.tenant_id=? AND completed.receivable_id=? AND completed.event_type=? AND completed.status<>'CANCELLED'
		  AND NOT EXISTS (
		    SELECT 1 FROM settlement_credit_sync compensating
		    WHERE compensating.tenant_id=completed.tenant_id AND compensating.event_type=? AND compensating.original_event_id=completed.event_id
		  )
		ORDER BY completed.created_at DESC,completed.id DESC LIMIT 1 FOR UPDATE`, tenant, receivableID, creditEventCompleted, creditEventReversed).Scan(&originalEventID, &eventType, &status, &settledAllocationID, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if eventType == creditEventReversed || status == "CANCELLED" {
		return "", nil
	}
	if status == "PENDING" || status == "RETRY_WAIT" {
		_, err = tx.ExecContext(ctx, `UPDATE settlement_credit_sync SET status='CANCELLED',updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND event_id=? AND status IN ('PENDING','RETRY_WAIT')`, tenant, originalEventID)
		return "", err
	}
	var original struct {
		CustomerID uint64 `json:"customer_id"`
		ContractNo string `json:"contract_no"`
		PeriodNo   string `json:"period_no"`
		DueDate    string `json:"due_date"`
		PaidDate   string `json:"paid_date"`
		DueAmount  string `json:"due_amount"`
	}
	if err = json.Unmarshal(payload, &original); err != nil {
		return "", err
	}
	reversalEventID := newID()
	reversalPayload, err := json.Marshal(map[string]any{
		"event_id": reversalEventID, "event_type": creditEventReversed, "tenant_id": tenant,
		"payment_id": reversalEventID, "reversal_of_payment_id": settledAllocationID,
		"customer_id": original.CustomerID, "contract_no": original.ContractNo,
		"period_no": original.PeriodNo, "due_date": original.DueDate, "paid_date": original.PaidDate,
		"due_amount": original.DueAmount, "paid_amount": "0.00", "source_system": "settlement",
	})
	if err != nil {
		return "", err
	}
	if err = insertCreditSyncEvent(ctx, tx, tenant, reversalEventID, allocationID, receivableID, creditEventReversed, originalEventID, reversalPayload); err != nil {
		return "", err
	}
	return reversalEventID, nil
}
