package service

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/j-s-te/settlement/internal/businessclock"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// FinanceDashboard 是结算权威事实的租户只读快照。
type FinanceDashboard struct {
	Available       bool                    `json:"available"`
	TenantID        string                  `json:"tenant_id"`
	SnapshotAt      string                  `json:"snapshot_at"`
	SourceUpdatedAt string                  `json:"source_updated_at,omitempty"`
	SourceCode      string                  `json:"source_code"`
	Metrics         FinanceDashboardMetrics `json:"metrics"`
	MissingMetrics  []string                `json:"missing_metrics"`
	Basis           string                  `json:"basis"`
}

type FinanceDashboardMetrics struct {
	Currencies []DashboardCurrencyTotals `json:"currencies"`
	Month      string                    `json:"month"`
	AsOfDate   string                    `json:"as_of_date"`
}

type DashboardCurrencyTotals struct {
	Currency           string   `json:"currency"`
	OriginalMinor      int64    `json:"original_minor"`
	AllocatedMinor     int64    `json:"allocated_minor"`
	OpenMinor          int64    `json:"open_minor"`
	Over90Minor        int64    `json:"over_90_minor"`
	MonthReceiptsMinor int64    `json:"month_receipts_minor"`
	CollectionRate     *float64 `json:"collection_rate"`
}

// DashboardFinance 以单个只读可重复读事务汇总已释放财务事实。
// 参数：ctx为取消上下文，tenant为签名身份租户，now为本次唯一取时。
// 返回值：提交后的按币种快照；金额溢出、源查询或提交失败时返回错误。
func (s *Service) DashboardFinance(ctx context.Context, tenant string, now time.Time) (FinanceDashboard, error) {
	empty := FinanceDashboard{}
	if tenant == "" || strings.TrimSpace(tenant) != tenant || now.IsZero() {
		return empty, ErrInvalid
	}
	if s == nil || s.DB == nil {
		return empty, ErrDependency
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	snapshot, err := readDashboardFinance(ctx, tx, tenant, now)
	if err != nil {
		return empty, err
	}
	if err = ctx.Err(); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	if err = ctx.Err(); err != nil {
		return empty, err
	}
	return snapshot, nil
}

// readDashboardFinance 使用同一北京时间日期生成账龄和月窗口。
// 参数：ctx为查询上下文，tx为只读事务，tenant为租户，now为唯一取时。
// 返回值：按币种快照；有界聚合读取或金额转换错误向上传递。
func readDashboardFinance(ctx context.Context, tx *sql.Tx, tenant string, now time.Time) (FinanceDashboard, error) {
	now = now.In(businessclock.Location())
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	value := FinanceDashboard{TenantID: tenant, SourceCode: "settlement", SnapshotAt: now.UTC().Format(time.RFC3339Nano),
		MissingMetrics: []string{"recognized_income", "department_cost"},
		Basis:          "按币种分别计算，金额为分，不换汇；CONFIRMED应收只计RELEASED合同快照，已核销取净received_allocated_amount，未清余额保留核销语义；账龄为北京时间到期超过90天的未清余额；本月有效且非HOLD回款不是收入确认。",
		Metrics:        FinanceDashboardMetrics{Currencies: []DashboardCurrencyTotals{}, Month: month.Format("2006-01"), AsOfDate: day.Format("2006-01-02")}}
	totals, updated, err := dashboardReceivableTotals(ctx, tx, tenant, day.AddDate(0, 0, -90).Format("2006-01-02"))
	if err != nil {
		return value, err
	}
	receipts, receiptUpdated, err := dashboardReceiptTotals(ctx, tx, tenant, month)
	if err != nil {
		return value, err
	}
	if receiptUpdated.After(updated) {
		updated = receiptUpdated
	}
	for currency, receipt := range receipts {
		row := totals[currency]
		row.Currency = currency
		row.MonthReceiptsMinor = receipt
		totals[currency] = row
	}
	for _, row := range totals {
		if row.OriginalMinor > 0 {
			rate := 100 * float64(row.AllocatedMinor) / float64(row.OriginalMinor)
			row.CollectionRate = &rate
		}
		value.Metrics.Currencies = append(value.Metrics.Currencies, row)
	}
	sort.Slice(value.Metrics.Currencies, func(i, j int) bool {
		return value.Metrics.Currencies[i].Currency < value.Metrics.Currencies[j].Currency
	})
	if !updated.IsZero() {
		value.SourceUpdatedAt = updated.UTC().Format(time.RFC3339Nano)
	}
	value.Available = true
	return value, nil
}

// dashboardReceivableTotals 读取已释放合同对应应收的精确汇总。
// 参数：ctx为上下文，tx为快照事务，tenant为租户，cutoff为90天截止日。
// 返回值：有界币种汇总及来源更新时间；SQL或精度异常时返回错误。
func dashboardReceivableTotals(ctx context.Context, tx *sql.Tx, tenant, cutoff string) (map[string]DashboardCurrencyTotals, time.Time, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.currency,CAST(SUM(r.original_amount) AS CHAR),
 CAST(SUM(r.received_allocated_amount) AS CHAR),CAST(SUM(r.open_amount) AS CHAR),
 CAST(SUM(CASE WHEN r.due_date < ? THEN r.open_amount ELSE 0 END) AS CHAR),
 MAX(GREATEST(r.updated_at,cs.updated_at))
 FROM settlement_receivable r
 JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id
 WHERE r.tenant_id=? AND r.recognition_status='CONFIRMED' AND cs.migration_state='RELEASED'
 GROUP BY r.currency ORDER BY r.currency LIMIT 257`, cutoff, tenant)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	result := map[string]DashboardCurrencyTotals{}
	latest := time.Time{}
	for rows.Next() {
		var currency string
		var amounts [4]string
		var updated any
		if err = rows.Scan(&currency, &amounts[0], &amounts[1], &amounts[2], &amounts[3], &updated); err != nil {
			return nil, latest, err
		}
		if len(result) >= 256 {
			return nil, latest, ErrInvalid
		}
		row := DashboardCurrencyTotals{Currency: currency}
		targets := []*int64{&row.OriginalMinor, &row.AllocatedMinor, &row.OpenMinor, &row.Over90Minor}
		for i, text := range amounts {
			*targets[i], err = dashboardMinor(text)
			if err != nil {
				return nil, latest, err
			}
		}
		result[currency] = row
		stamp, parseErr := dashboardSourceTime(updated)
		if parseErr != nil {
			return nil, latest, parseErr
		}
		if stamp.After(latest) {
			latest = stamp
		}
	}
	return result, latest, rows.Err()
}

// dashboardReceiptTotals 排除未释放导入回款并保留有效回款三态。
// 参数：ctx为上下文，tx为快照事务，tenant为租户，month为北京当月首日。
// 返回值：本月按币种回款与更新时间；SQL、边界或金额异常时返回错误。
func dashboardReceiptTotals(ctx context.Context, tx *sql.Tx, tenant string, month time.Time) (map[string]int64, time.Time, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rc.currency,CAST(SUM(rc.amount) AS CHAR),MAX(rc.updated_at)
 FROM settlement_receipt rc
 WHERE rc.tenant_id=? AND rc.status IN ('AVAILABLE','PARTIALLY_ALLOCATED','FULLY_ALLOCATED')
 AND rc.receipt_date >= ? AND rc.receipt_date < ?
 AND NOT EXISTS (SELECT 1 FROM settlement_import_fact f
 JOIN settlement_contract_snapshot cs ON cs.id=f.contract_snapshot_id AND cs.tenant_id=f.tenant_id
 WHERE f.tenant_id=rc.tenant_id AND f.target_business_id=rc.id AND f.fact_type='RECEIPT'
 AND cs.migration_state <> 'RELEASED')
 GROUP BY rc.currency ORDER BY rc.currency LIMIT 257`, tenant, month.Format("2006-01-02"), month.AddDate(0, 1, 0).Format("2006-01-02"))
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	result := map[string]int64{}
	latest := time.Time{}
	for rows.Next() {
		var currency, text string
		var updated any
		if err = rows.Scan(&currency, &text, &updated); err != nil {
			return nil, latest, err
		}
		if len(result) >= 256 {
			return nil, latest, ErrInvalid
		}
		result[currency], err = dashboardMinor(text)
		if err != nil {
			return nil, latest, err
		}
		stamp, parseErr := dashboardSourceTime(updated)
		if parseErr != nil {
			return nil, latest, parseErr
		}
		if stamp.After(latest) {
			latest = stamp
		}
	}
	return result, latest, rows.Err()
}

var dashboardDecimalPattern = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)

// dashboardMinor 将数据库十进制聚合精确转换为分，不经过浮点数。
// 参数：text为SQL DECIMAL的十进制字符串。
// 返回值：int64分值；非十进制、分以下精度或溢出时失败关闭。
func dashboardMinor(text string) (int64, error) {
	if !dashboardDecimalPattern.MatchString(text) {
		return 0, ErrInvalid
	}
	value, ok := new(big.Rat).SetString(text)
	if !ok {
		return 0, ErrInvalid
	}
	value.Mul(value, big.NewRat(100, 1))
	if !value.IsInt() || !value.Num().IsInt64() {
		return 0, fmt.Errorf("%w: dashboard monetary precision or overflow", ErrInvalid)
	}
	return value.Num().Int64(), nil
}

// dashboardSourceTime 兼容驱动返回的DATETIME聚合值并统一为UTC。
// 参数：value为数据库时间值、字节、字符串或空值。
// 返回值：真实来源时间；无法解释的非空时间返回错误而不伪造查询时间。
func dashboardSourceTime(value any) (time.Time, error) {
	switch stamp := value.(type) {
	case nil:
		return time.Time{}, nil
	case time.Time:
		return stamp.UTC(), nil
	case []byte:
		return dashboardSourceTime(string(stamp))
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999"} {
			if parsed, err := time.Parse(layout, stamp); err == nil {
				return parsed.UTC(), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("%w: invalid dashboard source timestamp", ErrInvalid)
}
