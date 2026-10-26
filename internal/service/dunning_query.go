package service

import (
	"context"
)

func (s *Service) DunningCoverage(ctx context.Context, p Principal) (DunningCoverage, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,version,aging_from_days,aging_to_days FROM settlement_dunning_policy WHERE tenant_id=? AND enabled=TRUE ORDER BY aging_from_days,aging_to_days,version,id`, p.TenantID)
	if err != nil {
		return DunningCoverage{}, err
	}
	defer rows.Close()
	out := DunningCoverage{Policies: []DunningCoveragePolicy{}, Gaps: []DunningCoverageGap{}}
	for rows.Next() {
		var policy DunningCoveragePolicy
		if err := rows.Scan(&policy.ID, &policy.Name, &policy.Version, &policy.AgingFromDays, &policy.AgingToDays); err != nil {
			return DunningCoverage{}, err
		}
		out.Policies = append(out.Policies, policy)
	}
	if err := rows.Err(); err != nil {
		return DunningCoverage{}, err
	}
	out.Gaps = dunningCoverageGaps(out.Policies)
	return out, nil
}

func (s *Service) AgingSummary(ctx context.Context, p Principal) ([]AgingBucket, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT currency,
		CASE WHEN DATEDIFF(CURDATE(),due_date)<=30 THEN '0-30'
		     WHEN DATEDIFF(CURDATE(),due_date)<=60 THEN '31-60'
		     WHEN DATEDIFF(CURDATE(),due_date)<=90 THEN '61-90'
		     ELSE '90+' END AS bucket,
		COALESCE(SUM(open_amount),0) AS amount, COUNT(*) AS count
		FROM settlement_receivable WHERE tenant_id=? AND open_amount>0
		GROUP BY currency,bucket
		ORDER BY currency, FIELD(bucket,'0-30','31-60','61-90','90+')`, p.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgingBucket{}
	for rows.Next() {
		var item AgingBucket
		if err := rows.Scan(&item.Currency, &item.Bucket, &item.Amount, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type DunningOverview struct {
	OverdueCount       int64                  `json:"overdue_count"`
	OverdueAmount      string                 `json:"overdue_amount"`
	CurrencyTotals     []DunningCurrencyTotal `json:"currency_totals"`
	ActiveCaseCount    int64                  `json:"active_case_count"`
	EscalatedCaseCount int64                  `json:"escalated_case_count"`
	ActionCount        int64                  `json:"action_count"`
	PendingActionCount int64                  `json:"pending_action_count"`
	UncoveredOverdue   int64                  `json:"uncovered_overdue_count"`
}

// DunningOverview 是催收页面使用的服务端聚合，确保
// KPI 卡片不会静默地只描述当前加载页。

func (s *Service) DunningOverview(ctx context.Context, p Principal) (DunningOverview, error) {
	var out DunningOverview
	err := s.DB.QueryRowContext(ctx, `SELECT
COALESCE(SUM(CASE WHEN r.open_amount>0 AND r.due_date<CURDATE() THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN r.open_amount>0 AND r.due_date<CURDATE() THEN r.open_amount ELSE 0 END),0),
(SELECT COUNT(*) FROM settlement_dunning_case dc WHERE dc.tenant_id=? AND dc.status='ACTIVE'),
(SELECT COUNT(*) FROM settlement_dunning_case dc WHERE dc.tenant_id=? AND dc.status='ACTIVE' AND dc.current_escalation_level>1),
(SELECT COUNT(*) FROM settlement_dunning_action da WHERE da.tenant_id=?),
(SELECT COUNT(*) FROM settlement_dunning_action da WHERE da.tenant_id=? AND da.status='PENDING'),
(SELECT COUNT(*) FROM settlement_receivable r2 WHERE r2.tenant_id=? AND r2.open_amount>0 AND r2.due_date<CURDATE() AND NOT EXISTS (
  SELECT 1 FROM settlement_dunning_policy p2 WHERE p2.tenant_id=r2.tenant_id AND p2.enabled=TRUE
    AND DATEDIFF(CURDATE(),r2.due_date) BETWEEN p2.aging_from_days AND p2.aging_to_days))
FROM settlement_receivable r WHERE r.tenant_id=?`, p.TenantID, p.TenantID, p.TenantID, p.TenantID, p.TenantID, p.TenantID).Scan(&out.OverdueCount, &out.OverdueAmount, &out.ActiveCaseCount, &out.EscalatedCaseCount, &out.ActionCount, &out.PendingActionCount, &out.UncoveredOverdue)
	if err != nil {
		return out, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT currency,COALESCE(SUM(open_amount),0) FROM settlement_receivable WHERE tenant_id=? AND open_amount>0 AND due_date<CURDATE() GROUP BY currency ORDER BY currency`, p.TenantID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.CurrencyTotals = []DunningCurrencyTotal{}
	for rows.Next() {
		var item DunningCurrencyTotal
		if err := rows.Scan(&item.Currency, &item.Overdue); err != nil {
			return out, err
		}
		out.CurrencyTotals = append(out.CurrencyTotals, item)
	}
	return out, rows.Err()
}
