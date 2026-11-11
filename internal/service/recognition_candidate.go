package service

import (
	"context"
	"fmt"
)

// recognitionClaim 精确读取同单据权威候选并校验组织范围，不接受项目投影副本。
// 参数：ctx/p为当前身份，in为显式候选引用。
// 返回值：完整权威单据；来源不可用、环境或范围漂移拒绝。
func (s *Service) recognitionClaim(ctx context.Context, p Principal, in RecognitionInput) (CostClaim, error) {
	if s.ReimbursementCosts == nil {
		return CostClaim{}, ErrDependency
	}
	snapshot, e := s.ReimbursementCosts.ReadCosts(ctx, p.TenantID, in.SourceClaimID)
	if e != nil {
		return CostClaim{}, ErrDependency
	}
	if e = validateCostSnapshot(snapshot, p.TenantID, in.SourceEnvironment, in.SourceClaimID); e != nil {
		return CostClaim{}, e
	}
	claim, e := costClaimByID(snapshot, in.SourceClaimID)
	if e != nil {
		return claim, e
	}
	for _, item := range claim.FrozenItems {
		if !recognitionAllowed(p, "settlement.recognition.read", item.OrganizationID) {
			return CostClaim{}, ErrForbidden
		}
	}
	if !recognitionAllowed(p, "settlement.recognition.read", claim.OrganizationID) {
		return CostClaim{}, ErrForbidden
	}
	return claim, nil
}

// recognitionRemaining 计算原确认在独立审核冲回后的净额与含税余额。
// 参数：original为原确认，records为完整同租户账本。
// 返回值：未税余额与含税余额，不改变原记录。
func recognitionRemaining(original Recognition, records []Recognition) (int64, int64) {
	net, gross := original.NetMinor, original.GrossMinor
	for _, row := range records {
		if row.Status == "CONFIRMED" && row.OriginalID == original.ID {
			net -= row.NetMinor
			gross -= row.GrossMinor
		}
	}
	return net, gross
}

// recognitionConflicts 查出已冲正付款仍有确认余额的原事实，冻结整张单后续确认。
// 参数：claim为完整权威历史，environment为当前来源环境，records为完整同租户账本。
// 返回值：真实未处置原事实；重付不会消除旧冲正差异。
func recognitionConflicts(claim CostClaim, environment string, records []Recognition) []Recognition {
	reversed := map[string]bool{}
	for _, effect := range claim.Effects {
		if effect.Kind == "PAYMENT_FULLY_REVERSED" {
			reversed[effect.ReversesEffectID] = true
		}
	}
	out := []Recognition{}
	for _, record := range records {
		if record.SourceEnvironment != environment || record.SourceSystem != "reimbursement" || record.SourceClaimID != claim.ClaimID || record.OriginalID != "" || record.Status != "CONFIRMED" || !reversed[record.SourceEffectID] {
			continue
		}
		net, gross := recognitionRemaining(record, records)
		if net > 0 || gross > 0 {
			out = append(out, record)
		}
	}
	return out
}

// checkRecognitionCandidate 核对冻结费用身份、当前有效付款与跨付款累计确认上限。
// 参数：p为当前主体，row为本次确认，claim为权威历史，records为同租户完整账本。
// 返回值：额度及状态满足才无错误，未处置差异或超额返回冲突。
func checkRecognitionCandidate(p Principal, row Recognition, claim CostClaim, records []Recognition) error {
	conflict := func(reason string) error { return fmt.Errorf("%w: %s", ErrConflict, reason) }
	if claim.ClaimID != row.SourceClaimID || claim.Status != "KNOWN" || row.Currency != claim.Currency || row.SourceSystem != "reimbursement" {
		return conflict("费用候选未知或身份不一致")
	}
	if len(recognitionConflicts(claim, row.SourceEnvironment, records)) > 0 {
		return conflict("同报销单存在未处置冲正差异，请先独立审核冲回原确认")
	}
	reversed := map[string]bool{}
	var effect CostEffect
	for _, item := range claim.Effects {
		if item.Kind == "PAYMENT_FULLY_REVERSED" {
			reversed[item.ReversesEffectID] = true
		}
		if item.EffectID == row.SourceEffectID {
			effect = item
		}
	}
	if effect.Kind != "PAYMENT_RECORDED" || reversed[effect.EffectID] {
		return conflict("原付款已冲正或不可确认")
	}
	var candidate CostItem
	for _, item := range claim.FrozenItems {
		if item.ApprovedContentHash == effect.ApprovedContentHash && item.ItemID == row.SourceItemID && item.AllocationKey == row.SourceAllocationKey {
			candidate = item
		}
	}
	if candidate.ItemID == "" || candidate.ProjectID != row.ProjectID || candidate.ServiceItemID != row.ServiceItemID || candidate.OccurredDate != row.OccurredDate {
		return conflict("费用项、项目、服务项或发生日不符批准快照")
	}
	if candidate.ExpenseNature == "PREPAYMENT" && len(row.PrepaymentBasis) < 6 {
		return conflict("预付须补充实际发生或摊销依据，不能直接转成本")
	}
	var claimUsed int64
	paidNet, err := costClaimNet(claim)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.SourceEnvironment == row.SourceEnvironment && record.SourceSystem == "reimbursement" && record.SourceClaimID == claim.ClaimID && record.Status == "CONFIRMED" && record.OriginalID == "" {
			_, remaining := recognitionRemaining(record, records)
			if remaining < 0 || remaining > RecognitionMaximum-claimUsed {
				return conflict("单据累计确认超限")
			}
			claimUsed += remaining
		}
	}
	if paidNet < 0 || row.GrossMinor > paidNet-claimUsed {
		return conflict("本单累计确认超过权威净付款额")
	}
	var used int64
	for _, record := range records {
		if record.SourceEnvironment != row.SourceEnvironment || record.SourceSystem != "reimbursement" || record.Status != "CONFIRMED" || record.OriginalID != "" || record.SourceClaimID != claim.ClaimID || costStableKey(record.SourceItemID, record.SourceAllocationKey) != costStableKey(row.SourceItemID, row.SourceAllocationKey) {
			continue
		}
		_, remaining := recognitionRemaining(record, records)
		if remaining < 0 || remaining > candidate.AmountMinor-used {
			return conflict("费用确认累计金额超限")
		}
		used += remaining
	}
	if used < 0 || row.GrossMinor > candidate.AmountMinor-used {
		return conflict("同费用条目及分摊累计确认超过权威净额")
	}
	return nil
}

// RecognitionCandidates 返回明确组织范围内的最小成本候选供财务选择。
// 参数：ctx/p为可信身份，claimID为必需的精确报销单据ID。
// 返回值：完整单据候选及当前差异，不向无权组织披露整单。
func (s *Service) RecognitionCandidates(ctx context.Context, p Principal, claimID string) (map[string]any, error) {
	if !recognitionAllowed(p, "settlement.recognition.record", "") || claimID == "" || len(claimID) > 64 {
		return nil, ErrForbidden
	}
	claim, e := s.recognitionClaim(ctx, p, RecognitionInput{SourceClaimID: claimID, SourceEnvironment: s.RecognitionEnvironment})
	if e != nil {
		return nil, e
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	records, e := recognitionRows(ctx, tx, p.TenantID)
	if e != nil {
		return nil, e
	}
	conflicts := recognitionConflicts(claim, s.RecognitionEnvironment, records)
	// 差异明细仍逐条执行当前范围；跨组织来源仅向有相同读取范围的人展示。
	ids := []string{}
	for _, record := range conflicts {
		if recognitionAllowed(p, "settlement.recognition.read", record.OrganizationID) {
			ids = append(ids, record.ID)
		}
	}
	return map[string]any{"claim": claim, "blocked": len(conflicts) > 0, "conflict_record_ids": ids, "basis": "付款只产生候选；预付需实际发生或摊销依据并独立确认"}, nil
}
