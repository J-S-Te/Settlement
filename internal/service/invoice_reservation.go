package service

import (
	"context"
	"database/sql"
)

// invoiceAvailableForUpdate 用途：以实际当前锁定读计算可开余额，避免RR旧快照漏算占用。
// 参数：ctx为上下文，tx为申请事务，tenant为可信租户，snapshot为合同，receivable为应收。
// 返回值：返回精确可开金额；不存在、矛盾余额或数据库故障返回错误。
func invoiceAvailableForUpdate(ctx context.Context, tx *sql.Tx, tenant, snapshot, receivable string) (string, error) {
	var original, invoiced string
	err := tx.QueryRowContext(ctx, `SELECT original_amount,invoiced_amount FROM settlement_receivable WHERE tenant_id=? AND id=? AND contract_snapshot_id=? AND recognition_status='CONFIRMED' FOR UPDATE`, tenant, receivable, snapshot).Scan(&original, &invoiced)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	available := add(original, "-"+invoiced)
	// 所有申请先锁同一应收，再对实际占用行执行当前读。
	// 不使用无锁SUM子查询，已建立的RR读视图不会漏掉前一事务的新占用。
	rows, err := tx.QueryContext(ctx, `SELECT reserved_amount-invoiced_amount-released_amount FROM settlement_invoice_request_allocation WHERE tenant_id=? AND receivable_id=? AND status='RESERVED' ORDER BY id FOR UPDATE`, tenant, receivable)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			return "", err
		}
		reserved := add(value)
		if reserved.Sign() < 0 {
			return "", ErrConflict
		}
		available.Sub(available, reserved)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if available.Sign() < 0 {
		return "", ErrConflict
	}
	return available.FloatString(2), nil
}
