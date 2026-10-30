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

// AgingSummary 用途：按配置的五档账龄和币种汇总未结清应收。
// 参数：ctx为上下文，p为可信报表主体。
// 返回值：返回各币种分档金额和笔数，规则或存储失败返回错误。
func (s *Service) AgingSummary(ctx context.Context, p Principal) ([]AgingBucket, error) {
	days, err := s.AgingDays(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	bucket, err := BuildAgingCase(days, "due_date")
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT currency,`+bucket+` AS bucket,COALESCE(SUM(open_amount),0),COUNT(*) FROM settlement_receivable WHERE tenant_id=? AND EXISTS(SELECT 1 FROM settlement_contract_snapshot cs WHERE cs.tenant_id=settlement_receivable.tenant_id AND cs.id=settlement_receivable.contract_snapshot_id AND cs.migration_state='RELEASED') AND open_amount>0 GROUP BY currency,bucket ORDER BY currency,MIN(DATEDIFF(UTC_DATE(),due_date))`, p.TenantID)
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

// DunningOverview 仅汇总已放行财务对应的经营催收指标。
// 参数：ctx为上下文，p为可信租户主体。
// 返回值：逐币种金额及动作统计；数据库故障返回错误。
func (s *Service) DunningOverview(ctx context.Context, p Principal) (DunningOverview, error) {
	var out DunningOverview
	err := s.DB.QueryRowContext(ctx, `SELECT
COALESCE(SUM(CASE WHEN r.open_amount>0 AND r.due_date<CURDATE() THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN r.open_amount>0 AND r.due_date<CURDATE() THEN r.open_amount ELSE 0 END),0),
(SELECT COUNT(*) FROM settlement_dunning_case dc WHERE dc.tenant_id=? AND (EXISTS(SELECT 1 FROM settlement_receivable import_r JOIN settlement_contract_snapshot import_cs ON import_cs.id=import_r.contract_snapshot_id AND import_cs.tenant_id=import_r.tenant_id WHERE import_r.id=dc.receivable_id AND import_r.tenant_id=dc.tenant_id AND import_cs.migration_state='RELEASED') OR EXISTS(SELECT 1 FROM settlement_receivable_plan import_p JOIN settlement_contract_snapshot import_cs ON import_cs.id=import_p.contract_snapshot_id AND import_cs.tenant_id=import_p.tenant_id WHERE import_p.id=dc.plan_id AND import_p.tenant_id=dc.tenant_id AND import_cs.migration_state='RELEASED')) AND dc.status='ACTIVE'),
(SELECT COUNT(*) FROM settlement_dunning_case dc WHERE dc.tenant_id=? AND (EXISTS(SELECT 1 FROM settlement_receivable import_r JOIN settlement_contract_snapshot import_cs ON import_cs.id=import_r.contract_snapshot_id AND import_cs.tenant_id=import_r.tenant_id WHERE import_r.id=dc.receivable_id AND import_r.tenant_id=dc.tenant_id AND import_cs.migration_state='RELEASED') OR EXISTS(SELECT 1 FROM settlement_receivable_plan import_p JOIN settlement_contract_snapshot import_cs ON import_cs.id=import_p.contract_snapshot_id AND import_cs.tenant_id=import_p.tenant_id WHERE import_p.id=dc.plan_id AND import_p.tenant_id=dc.tenant_id AND import_cs.migration_state='RELEASED')) AND dc.status='ACTIVE' AND dc.current_escalation_level>1),
(SELECT COUNT(*) FROM settlement_dunning_action da JOIN settlement_dunning_case dc ON dc.id=da.dunning_case_id AND dc.tenant_id=da.tenant_id WHERE da.tenant_id=? AND (EXISTS(SELECT 1 FROM settlement_receivable import_r JOIN settlement_contract_snapshot import_cs ON import_cs.id=import_r.contract_snapshot_id AND import_cs.tenant_id=import_r.tenant_id WHERE import_r.id=dc.receivable_id AND import_r.tenant_id=dc.tenant_id AND import_cs.migration_state='RELEASED') OR EXISTS(SELECT 1 FROM settlement_receivable_plan import_p JOIN settlement_contract_snapshot import_cs ON import_cs.id=import_p.contract_snapshot_id AND import_cs.tenant_id=import_p.tenant_id WHERE import_p.id=dc.plan_id AND import_p.tenant_id=dc.tenant_id AND import_cs.migration_state='RELEASED'))),
(SELECT COUNT(*) FROM settlement_dunning_action da JOIN settlement_dunning_case dc ON dc.id=da.dunning_case_id AND dc.tenant_id=da.tenant_id WHERE da.tenant_id=? AND (EXISTS(SELECT 1 FROM settlement_receivable import_r JOIN settlement_contract_snapshot import_cs ON import_cs.id=import_r.contract_snapshot_id AND import_cs.tenant_id=import_r.tenant_id WHERE import_r.id=dc.receivable_id AND import_r.tenant_id=dc.tenant_id AND import_cs.migration_state='RELEASED') OR EXISTS(SELECT 1 FROM settlement_receivable_plan import_p JOIN settlement_contract_snapshot import_cs ON import_cs.id=import_p.contract_snapshot_id AND import_cs.tenant_id=import_p.tenant_id WHERE import_p.id=dc.plan_id AND import_p.tenant_id=dc.tenant_id AND import_cs.migration_state='RELEASED')) AND da.status='PENDING'),
(SELECT COUNT(*) FROM settlement_receivable r2 WHERE r2.tenant_id=? AND EXISTS(SELECT 1 FROM settlement_contract_snapshot cs WHERE cs.id=r2.contract_snapshot_id AND cs.tenant_id=r2.tenant_id AND cs.migration_state='RELEASED') AND r2.open_amount>0 AND r2.due_date<CURDATE() AND NOT EXISTS (
  SELECT 1 FROM settlement_dunning_policy p2 WHERE p2.tenant_id=r2.tenant_id AND p2.enabled=TRUE
    AND DATEDIFF(CURDATE(),r2.due_date) BETWEEN p2.aging_from_days AND p2.aging_to_days))
FROM settlement_receivable r WHERE r.tenant_id=? AND EXISTS(SELECT 1 FROM settlement_contract_snapshot cs WHERE cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id AND cs.migration_state='RELEASED')`, p.TenantID, p.TenantID, p.TenantID, p.TenantID, p.TenantID, p.TenantID).Scan(&out.OverdueCount, &out.OverdueAmount, &out.ActiveCaseCount, &out.EscalatedCaseCount, &out.ActionCount, &out.PendingActionCount, &out.UncoveredOverdue)
	if err != nil {
		return out, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT currency,COALESCE(SUM(open_amount),0) FROM settlement_receivable WHERE tenant_id=? AND EXISTS(SELECT 1 FROM settlement_contract_snapshot cs WHERE cs.tenant_id=settlement_receivable.tenant_id AND cs.id=settlement_receivable.contract_snapshot_id AND cs.migration_state='RELEASED') AND open_amount>0 AND due_date<CURDATE() GROUP BY currency ORDER BY currency`, p.TenantID)
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
