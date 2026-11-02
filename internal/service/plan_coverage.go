package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
)

// 计划覆盖守卫的阈值来源。默认值写死在代码里，环境变量只用于财务确认后
// 收紧或放宽阈值；非法值或越界值一律回到默认阈值，绝不因为配置错误关闭守卫。
const (
	planCoverageGapEnvName  = "SETTLEMENT_PLAN_COVERAGE_GAP_PERCENT"
	planCoverageDropEnvName = "SETTLEMENT_PLAN_COVERAGE_DROP_PERCENT"
	planCoverageGapDefault  = 5
	planCoverageDropDefault = 50
)

// 差额触发原因：合计差额与单期降幅可以同时命中。
const (
	planCoverageTriggerGap  = "COVERAGE_GAP"
	planCoverageTriggerDrop = "SINGLE_INSTALLMENT_DROP"
)

// PlanCoverageView 是计划覆盖的只读视图：合同金额、计划合计、差额与阈值判定。
// 差额为正表示计划少计应收（合同金额大于计划合计），为负表示计划超计。
type PlanCoverageView struct {
	ContractSnapshotID string   `json:"contract_snapshot_id"`
	ContractNo         string   `json:"contract_no"`
	ContractAmount     string   `json:"contract_amount"`
	PlanTotal          string   `json:"plan_total"`
	Gap                string   `json:"gap"`
	GapPercent         string   `json:"gap_percent"`
	ExceedsThreshold   bool     `json:"exceeds_threshold"`
	Triggered          []string `json:"triggered"`
	// ContractEventPlan 表示该合同的计划来自合同财务事件基线，
	// 其合计本应等于合同金额；人工改动造成的差额必须留审计。
	ContractEventPlan bool `json:"contract_event_plan"`
}

// planCoverage 是一次"合同金额对计划合计"的锁定读结果，供守卫与审计使用。
type planCoverage struct {
	ContractSnapshotID string
	ContractNo         string
	ContractAmount     string
	PlanTotal          string
	Gap                string
	GapPercent         string
	DropPercent        string
	Exceeds            bool
	Triggered          []string
	ContractEventPlans int
}

// view 把内部覆盖事实收敛为HTTP只读结构。
// 参数：无。
// 返回值：不可变的只读视图。
func (c planCoverage) view() PlanCoverageView {
	triggered := append([]string{}, c.Triggered...)
	return PlanCoverageView{
		ContractSnapshotID: c.ContractSnapshotID,
		ContractNo:         c.ContractNo,
		ContractAmount:     c.ContractAmount,
		PlanTotal:          c.PlanTotal,
		Gap:                c.Gap,
		GapPercent:         c.GapPercent,
		ExceedsThreshold:   c.Exceeds,
		Triggered:          triggered,
		ContractEventPlan:  c.ContractEventPlans > 0,
	}
}

// planCoveragePercentLimit 读取百分比阈值；空值、非法值或越界值回到默认阈值。
// 参数：name为环境变量名，fallback为默认百分比。
// 返回值：0 到 100 之间的阈值有理数。
func planCoveragePercentLimit(name string, fallback int) *big.Rat {
	if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
		if value, ok := new(big.Rat).SetString(raw); ok && value.Sign() >= 0 && value.Cmp(big.NewRat(100, 1)) <= 0 {
			return value
		}
	}
	return big.NewRat(int64(fallback), 1)
}

// planCoverageThresholds 返回当前生效的差额与单期降幅阈值百分比。
// 参数：无。
// 返回值：差额阈值与降幅阈值，均不超过 100%。
func planCoverageThresholds() (gap, drop *big.Rat) {
	return planCoveragePercentLimit(planCoverageGapEnvName, planCoverageGapDefault),
		planCoveragePercentLimit(planCoverageDropEnvName, planCoverageDropDefault)
}

// evaluatePlanCoverage 计算人工改动后的计划合计、差额与阈值命中，不访问数据库。
// 参数：contractAmount为合同金额，existingTotal为除本次改动计划外的合计，
// planned为改动后金额，previous为改动前金额（新建时为空），contractEventPlans为合同事件来源计划数。
// 返回值：覆盖事实；金额不可解析时返回非法参数错误。
func evaluatePlanCoverage(contractAmount, existingTotal, planned, previous string, contractEventPlans int) (planCoverage, error) {
	contract, ok := new(big.Rat).SetString(strings.TrimSpace(contractAmount))
	if !ok {
		return planCoverage{}, fmt.Errorf("%w: contract amount is not a decimal", ErrInvalid)
	}
	existing, ok := new(big.Rat).SetString(strings.TrimSpace(existingTotal))
	if !ok {
		return planCoverage{}, fmt.Errorf("%w: plan total is not a decimal", ErrInvalid)
	}
	next, ok := new(big.Rat).SetString(strings.TrimSpace(planned))
	if !ok {
		return planCoverage{}, fmt.Errorf("%w: planned amount is not a decimal", ErrInvalid)
	}
	total := new(big.Rat).Add(existing, next)
	gap := new(big.Rat).Sub(contract, total)
	gapPercent := new(big.Rat)
	if contract.Sign() > 0 {
		absolute := new(big.Rat).Abs(gap)
		gapPercent.Quo(absolute.Mul(absolute, big.NewRat(100, 1)), contract)
	}
	gapLimit, dropLimit := planCoverageThresholds()
	triggered := []string{}
	if gapPercent.Cmp(gapLimit) > 0 {
		triggered = append(triggered, planCoverageTriggerGap)
	}
	dropPercent := ""
	if trimmed := strings.TrimSpace(previous); trimmed != "" {
		if before, ok := new(big.Rat).SetString(trimmed); ok && before.Sign() > 0 && next.Cmp(before) < 0 {
			drop := new(big.Rat).Quo(new(big.Rat).Mul(new(big.Rat).Sub(before, next), big.NewRat(100, 1)), before)
			dropPercent = drop.FloatString(2)
			if drop.Cmp(dropLimit) > 0 {
				triggered = append(triggered, planCoverageTriggerDrop)
			}
		}
	}
	return planCoverage{
		ContractAmount:     contract.FloatString(2),
		PlanTotal:          total.FloatString(2),
		Gap:                gap.FloatString(2),
		GapPercent:         gapPercent.FloatString(2),
		DropPercent:        dropPercent,
		Exceeds:            len(triggered) > 0,
		Triggered:          triggered,
		ContractEventPlans: contractEventPlans,
	}, nil
}

// inspectPlanCoverage 用途：在同一把合同锁内读取合同金额与计划合计并计算差额。
// 参数：ctx为上下文，tx为业务事务，tenant为可信租户，snapshot为合同快照，
// excluded为被替换计划，planned为新金额，previous为改动前金额。
// 返回值：覆盖事实；合同不可见返回不存在，其余返回存储或参数错误。
func inspectPlanCoverage(ctx context.Context, tx *sql.Tx, tenant, snapshot, excluded, planned, previous string) (planCoverage, error) {
	var contractAmount, contractNo string
	err := tx.QueryRowContext(ctx, `SELECT contract_amount,source_contract_no FROM settlement_contract_snapshot WHERE tenant_id=? AND id=? AND financial_status='EFFECTIVE' FOR UPDATE`, tenant, snapshot).Scan(&contractAmount, &contractNo)
	if errors.Is(err, sql.ErrNoRows) {
		return planCoverage{}, ErrNotFound
	}
	if err != nil {
		return planCoverage{}, err
	}
	var existingTotal string
	var contractEventPlans int
	// 与被替换计划同表的单次扫描同时给出"其余计划合计"和"合同事件来源计划数"，
	// 既避免二次 SUM 造成的读视图漂移，也保证阈值判定与审计共用同一组事实。
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN id<>? THEN planned_amount ELSE 0 END),0),COALESCE(SUM(CASE WHEN generation_source='CONTRACT_EVENT' THEN 1 ELSE 0 END),0) FROM settlement_receivable_plan WHERE tenant_id=? AND contract_snapshot_id=?`, excluded, tenant, snapshot).Scan(&existingTotal, &contractEventPlans); err != nil {
		return planCoverage{}, err
	}
	coverage, err := evaluatePlanCoverage(contractAmount, existingTotal, planned, previous, contractEventPlans)
	if err != nil {
		return planCoverage{}, err
	}
	coverage.ContractSnapshotID, coverage.ContractNo = snapshot, contractNo
	return coverage, nil
}

// requirePlanCoverage 用途：串行校验计划合计上限与差额阈值，必要时要求财务 override。
// 参数：ctx为上下文，tx为业务事务，p为可信主体，snapshot为合同快照，excluded为被替换计划，
// planned为新金额，previous为改动前金额。
// 返回值：覆盖事实；超合同金额或超阈值且无 override 权限返回冲突。
func (s *Service) requirePlanCoverage(ctx context.Context, tx *sql.Tx, p Principal, snapshot, excluded, planned, previous string) (planCoverage, error) {
	coverage, err := inspectPlanCoverage(ctx, tx, p.TenantID, snapshot, excluded, planned, previous)
	if err != nil {
		return coverage, err
	}
	// 合同金额上限保持原语义：合计不得超过合同金额，任何权限都不放松。
	if decimalGreater(coverage.PlanTotal, coverage.ContractAmount) {
		return coverage, fmt.Errorf("%w: 计划合计不能超过合同金额", ErrConflict)
	}
	if !coverage.Exceeds {
		return coverage, nil
	}
	if !p.Allows("settlement.finance.override") {
		gapLimit, dropLimit := planCoverageThresholds()
		return coverage, fmt.Errorf("%w: 计划覆盖差额超过阈值（差额 %s / %s%%，单期降幅 %s%%，阈值 %s%% / %s%%），需要财务负责人 override 权限", ErrConflict,
			coverage.Gap, coverage.GapPercent, coverage.DropPercent, gapLimit.FloatString(2), dropLimit.FloatString(2))
	}
	return coverage, nil
}

// auditPlanCoverageDeviation 用途：把计划合计与合同金额的差额留成显式审计事实。
// 差额为零时不写审计；超阈值时风险等级为 HIGH，否则 MEDIUM。
// 参数：ctx为上下文，tx为业务事务，p为可信主体，coverage为覆盖事实，reason为改动原因，operation为入口。
// 返回值：审计入队失败返回错误；无差额时无错误。
func auditPlanCoverageDeviation(ctx context.Context, tx *sql.Tx, p Principal, coverage planCoverage, reason, operation string) error {
	gap, ok := new(big.Rat).SetString(coverage.Gap)
	if !ok || gap.Sign() == 0 {
		return nil
	}
	gapLimit, dropLimit := planCoverageThresholds()
	risk := "MEDIUM"
	if coverage.Exceeds {
		risk = "HIGH"
	}
	return outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_PLAN_COVERAGE_DEVIATION", "contract_snapshot", coverage.ContractSnapshotID, map[string]any{
		"actor_id":               p.UserID,
		"operation":              operation,
		"contract_no":            coverage.ContractNo,
		"contract_amount":        coverage.ContractAmount,
		"plan_total":             coverage.PlanTotal,
		"gap":                    coverage.Gap,
		"gap_percent":            coverage.GapPercent,
		"drop_percent":           coverage.DropPercent,
		"gap_threshold_percent":  gapLimit.FloatString(2),
		"drop_threshold_percent": dropLimit.FloatString(2),
		"triggered":              coverage.Triggered,
		"contract_event_plan":    coverage.ContractEventPlans > 0,
		"reason":                 reason,
		"result":                 "SUCCESS",
		"risk_level":             risk,
	})
}

// PlanCoverage 用途：返回单个合同的计划覆盖与差额，供台账持续可见。
// 参数：ctx为上下文，p为可信主体，snapshot为合同快照。
// 返回值：覆盖视图；无权、越范围或不存在返回错误。
func (s *Service) PlanCoverage(ctx context.Context, p Principal, snapshot string) (PlanCoverageView, error) {
	if !p.Allows("settlement.receivable.read") {
		return PlanCoverageView{}, ErrForbidden
	}
	if strings.TrimSpace(snapshot) == "" {
		return PlanCoverageView{}, fmt.Errorf("%w: contract_snapshot_id is required", ErrInvalid)
	}
	if err := s.RequireContractAccess(ctx, p, "settlement.receivable.read", snapshot); err != nil {
		return PlanCoverageView{}, err
	}
	var contractNo, contractAmount, total string
	var contractEventPlans int
	err := s.DB.QueryRowContext(ctx, `SELECT cs.source_contract_no,cs.contract_amount,COALESCE(SUM(rp.planned_amount),0),COALESCE(SUM(CASE WHEN rp.generation_source='CONTRACT_EVENT' THEN 1 ELSE 0 END),0) FROM settlement_contract_snapshot cs LEFT JOIN settlement_receivable_plan rp ON rp.tenant_id=cs.tenant_id AND rp.contract_snapshot_id=cs.id WHERE cs.tenant_id=? AND cs.id=? AND cs.financial_status='EFFECTIVE' GROUP BY cs.id,cs.source_contract_no,cs.contract_amount`, p.TenantID, snapshot).Scan(&contractNo, &contractAmount, &total, &contractEventPlans)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanCoverageView{}, ErrNotFound
	}
	if err != nil {
		return PlanCoverageView{}, err
	}
	coverage, err := evaluatePlanCoverage(contractAmount, total, "0", "", contractEventPlans)
	if err != nil {
		return PlanCoverageView{}, err
	}
	coverage.ContractSnapshotID, coverage.ContractNo = snapshot, contractNo
	return coverage.view(), nil
}

// ListPlanCoverageDeviations 用途：列出可见范围内计划合计不等于合同金额的合同。
// 参数：ctx为上下文，p为可信主体。
// 返回值：按差额由大到小排列的有界视图列表；无权返回禁止。
func (s *Service) ListPlanCoverageDeviations(ctx context.Context, p Principal) ([]PlanCoverageView, error) {
	if !p.Allows("settlement.receivable.read") {
		return nil, ErrForbidden
	}
	clause, scopeArgs := p.ContractPredicate("settlement.receivable.read", "cs")
	args := append([]any{p.TenantID}, scopeArgs...)
	rows, err := s.DB.QueryContext(ctx, `SELECT cs.id,cs.source_contract_no,cs.contract_amount,COALESCE(SUM(rp.planned_amount),0),COALESCE(SUM(CASE WHEN rp.generation_source='CONTRACT_EVENT' THEN 1 ELSE 0 END),0) FROM settlement_contract_snapshot cs LEFT JOIN settlement_receivable_plan rp ON rp.tenant_id=cs.tenant_id AND rp.contract_snapshot_id=cs.id WHERE cs.tenant_id=? AND cs.financial_status='EFFECTIVE' AND `+clause+` GROUP BY cs.id,cs.source_contract_no,cs.contract_amount HAVING COALESCE(SUM(rp.planned_amount),0)<>cs.contract_amount ORDER BY ABS(cs.contract_amount-COALESCE(SUM(rp.planned_amount),0)) DESC,cs.id DESC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []PlanCoverageView{}
	for rows.Next() {
		var snapshot, contractNo, contractAmount, total string
		var contractEventPlans int
		if err = rows.Scan(&snapshot, &contractNo, &contractAmount, &total, &contractEventPlans); err != nil {
			return nil, err
		}
		coverage, err := evaluatePlanCoverage(contractAmount, total, "0", "", contractEventPlans)
		if err != nil {
			return nil, err
		}
		coverage.ContractSnapshotID, coverage.ContractNo = snapshot, contractNo
		items = append(items, coverage.view())
	}
	return items, rows.Err()
}
