package service

import (
	"context"
	"database/sql"
	"encoding/json"
)

// recognitionClaimRows 精确读取来源单据已确认事实及其冲回链，复用来源与原确认索引。
// 参数：ctx/tx为持有租户账本锁的事务，tenant/claim为可信租户与当前来源单据。
// 返回值：完整相关确认链；数据库或记录身份损坏返回错误，不受无关草稿数量影响。
func recognitionClaimRows(ctx context.Context, tx *sql.Tx, tenant, claim string) ([]Recognition, error) {
	return recognitionPreciseRows(ctx, tx, tenant, `SELECT payload FROM settlement_recognition WHERE tenant_id=? AND source_claim_id=? AND status='CONFIRMED' UNION ALL SELECT r.payload FROM settlement_recognition o JOIN settlement_recognition r ON r.tenant_id=o.tenant_id AND r.original_id=o.id AND r.status='CONFIRMED' WHERE o.tenant_id=? AND o.source_claim_id=? AND o.original_id='' AND o.status='CONFIRMED' AND r.source_claim_id<>?`, tenant, claim, tenant, claim, claim)
}

// recognitionReversalRows 只读取当前原确认的已确认冲回，不装载全租户账本。
// 参数：ctx/tx为持有租户串行锁的事务，tenant/original为可信原确认身份。
// 返回值：完整冲回链；查询或载荷身份损坏时返回错误。
func recognitionReversalRows(ctx context.Context, tx *sql.Tx, tenant, original string) ([]Recognition, error) {
	return recognitionPreciseRows(ctx, tx, tenant, `SELECT payload FROM settlement_recognition WHERE tenant_id=? AND original_id=? AND status='CONFIRMED'`, tenant, original)
}

// recognitionPreciseRows 解码精确索引查询的完整结果，不使用全租户快照阈值。
// 参数：ctx/tx为当前事务，tenant为可信租户，query/args为内部固定SQL与绑定值。
// 返回值：全部相关事实；损坏或跨租户载荷拒绝处理。
func recognitionPreciseRows(ctx context.Context, tx *sql.Tx, tenant, query string, args ...any) ([]Recognition, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Recognition{}
	for rows.Next() {
		var raw []byte
		var row Recognition
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if json.Unmarshal(raw, &row) != nil || row.TenantID != tenant {
			return nil, ErrDependency
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
