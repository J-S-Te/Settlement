package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// enqueueAdjustedPlanCredit 将计划调整的结清转换写入同一财务事务。
// 参数：ctx/tx为事务，p为主体，snapshot/receivable为归属，settled为调整后状态。
// 返回值：新事件标识；非现金结清不生成信用付款，存储失败返回错误。
func (s *Service) enqueueAdjustedPlanCredit(ctx context.Context, tx *sql.Tx,
	p Principal, snapshot, receivable string, settled bool,
) (string, error) {
	if !settled {
		return enqueueCreditReversal(ctx, tx, p.TenantID, "", receivable)
	}
	var customer, contract, dueDate, original, received string
	var installment int
	err := tx.QueryRowContext(ctx, `SELECT cs.customer_id,cs.source_contract_no,
	 DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.original_amount,r.received_allocated_amount,
	 rp.installment_no FROM settlement_receivable r
	 JOIN settlement_contract_snapshot cs ON cs.tenant_id=r.tenant_id AND cs.id=r.contract_snapshot_id
	 JOIN settlement_receivable_plan rp ON rp.tenant_id=r.tenant_id AND rp.id=r.receivable_plan_id
	 WHERE r.tenant_id=? AND r.id=?`, p.TenantID, receivable).
		Scan(&customer, &contract, &dueDate, &original, &received, &installment)
	if err != nil {
		return "", err
	}
	// 核销减免只能消除欠款，不能冒充客户已支付的现金信用事实。
	if add(received).Sign() <= 0 || add(received).Cmp(add(original)) != 0 {
		return "", nil
	}
	eligible, err := s.creditEligibleSnapshot(ctx, tx, p.TenantID, snapshot, customer)
	if err != nil || !eligible {
		return "", err
	}
	return enqueueAdjustedCashCompletion(ctx, tx, p.TenantID, receivable,
		customer, contract, dueDate, original, received, installment)
}

// enqueueAdjustedCashCompletion 以真实有效核销和当前应收生成调整后的独立付款事实。
// 参数：ctx/tx为事务，tenant/receivable为范围，其余为锁定应收的业务字段。
// 返回值：新完成事件标识；无真实核销时不生成，查询或入队失败返回错误。
func enqueueAdjustedCashCompletion(ctx context.Context, tx *sql.Tx,
	tenant, receivable, customer, contract, dueDate, original, received string, installment int,
) (string, error) {
	var allocation, receipt, paidDate string
	err := tx.QueryRowContext(ctx, `SELECT a.id,a.receipt_id,DATE_FORMAT(r.receipt_date,'%Y-%m-%d')
	 FROM settlement_receipt_allocation a JOIN settlement_receipt r
	 ON r.tenant_id=a.tenant_id AND r.id=a.receipt_id
	 WHERE a.tenant_id=? AND a.receivable_id=? AND a.status IN ('CONFIRMED','PARTIALLY_REVERSED')
	 ORDER BY r.receipt_date DESC,a.confirmed_at DESC,a.id DESC LIMIT 1`, tenant, receivable).
		Scan(&allocation, &receipt, &paidDate)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	customerID, err := strconv.ParseUint(strings.TrimSpace(customer), 10, 64)
	if err != nil {
		return "", nil
	}
	eventID := newID()
	payload, err := json.Marshal(map[string]any{
		"event_id": eventID, "event_type": creditEventCompleted, "tenant_id": tenant,
		"payment_id": eventID, "receipt_id": receipt, "allocation_id": allocation,
		"customer_id": customerID, "contract_no": contract, "period_no": strconv.Itoa(installment),
		"due_date": creditDate(dueDate), "paid_date": creditDate(paidDate),
		"due_amount": original, "paid_amount": received, "source_system": "settlement",
	})
	if err != nil {
		return "", err
	}
	return eventID, insertCreditSync(ctx, tx, tenant, eventID, allocation, receivable, payload)
}
