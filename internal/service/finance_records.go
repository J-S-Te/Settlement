package service

import (
	"context"
	"strings"
)

// FinanceRecordQuery 表达财务列表有界分页及可选筛选。
type FinanceRecordQuery struct {
	Limit   int
	Offset  int
	Status  string
	Keyword string
}

// financeRecordQuery 校验服务调用的分页和状态筛选，保留无查询参数调用。
// 参数：vouchers 是否为凭证列表，inputs 可选的唯一查询条件。
// 返回值：规范化查询，参数越界或状态未知时返回 ErrInvalid。
func financeRecordQuery(vouchers bool, inputs []FinanceRecordQuery) (FinanceRecordQuery, error) {
	query := FinanceRecordQuery{Limit: 100}
	if len(inputs) > 1 {
		return query, ErrInvalid
	}
	if len(inputs) == 1 {
		query = inputs[0]
	}
	query.Status = strings.TrimSpace(query.Status)
	query.Keyword = strings.TrimSpace(query.Keyword)
	if query.Limit < 1 || query.Limit > 100 || query.Offset < 0 || query.Offset > 100000 || len(query.Keyword) > 128 {
		return query, ErrInvalid
	}
	statuses := map[string]bool{"HELD": true, "REVIEW_PENDING": true, "APPROVED": true, "REFUND_PENDING": true, "REFUND_CONFIRMED": true}
	if vouchers {
		statuses = map[string]bool{"NOT_CONFIGURED": true, "FAILED": true, "SENT": true}
	}
	if query.Status != "" && !statuses[query.Status] {
		return query, ErrInvalid
	}
	return query, nil
}

// ListFinanceRecords 返回受财务权限及租户边界控制的有界稳定列表。
// 参数：ctx 上下文，p 财务主体，vouchers 是否读取凭证，inputs 可选查询条件。
// 返回值：当前页投影；授权、筛选或持久化失败时返回错误。
func (s *Service) ListFinanceRecords(ctx context.Context, p Principal, vouchers bool, inputs ...FinanceRecordQuery) ([]map[string]any, error) {
	permission := "settlement.difference.manage"
	query := `SELECT id,receipt_id,kind,amount,reason,status,resolution,version,requested_by,
 refund_reference,
 DATE_FORMAT(refund_completed_at,'%Y-%m-%dT%H:%i:%s.%fZ') AS refund_completed_at,
 refund_confirmed_by,refund_reason
 FROM settlement_receipt_difference WHERE tenant_id=?`
	search := ` AND (LOCATE(?,id)>0 OR LOCATE(?,receipt_id)>0 OR LOCATE(?,reason)>0 OR LOCATE(?,refund_reference)>0)`
	if vouchers {
		permission = "settlement.voucher.manage"
		query = `SELECT id,writeoff_id,status,external_no,version FROM settlement_accounting_voucher WHERE tenant_id=?`
		search = ` AND (LOCATE(?,id)>0 OR LOCATE(?,writeoff_id)>0 OR LOCATE(?,external_no)>0)`
	}
	if err := financeAccess(p, permission); err != nil {
		return nil, err
	}
	filter, err := financeRecordQuery(vouchers, inputs)
	if err != nil {
		return nil, err
	}
	args := []any{p.TenantID}
	if filter.Status != "" {
		query += " AND status=?"
		args = append(args, filter.Status)
	}
	if filter.Keyword != "" {
		query += search
		for i := 0; i < strings.Count(search, "?"); i++ {
			args = append(args, filter.Keyword)
		}
	}
	query += " ORDER BY created_at DESC,id LIMIT ? OFFSET ?"
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err = rows.Scan(targets...); err != nil {
			return nil, err
		}
		item := map[string]any{}
		for i, name := range columns {
			if b, ok := values[i].([]byte); ok {
				values[i] = string(b)
			}
			item[name] = values[i]
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
