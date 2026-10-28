package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// CompleteDunningAction 用途：核对收件人与现行合同范围后完成催收反馈。
// 参数：ctx为上下文，p为主体，actionID为动作，version为版本，note为反馈。
// 返回值：成功无错误；失权、过期状态或存储失败返回错误。
func (s *Service) CompleteDunningAction(ctx context.Context, p Principal, actionID string, version uint64, note string) error {
	actionID = strings.TrimSpace(actionID)
	note = strings.TrimSpace(note)
	if actionID == "" || version == 0 || len([]rune(note)) > DunningActionNoteLimit {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var recipient, status string
	var currentVersion uint64
	err = tx.QueryRowContext(ctx, `SELECT recipient_user_id,status,version FROM settlement_dunning_action WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, actionID).Scan(&recipient, &status, &currentVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if recipient != p.UserID && !p.Allows("settlement.admin") {
		return ErrForbidden
	}
	if !p.Unrestricted("settlement.dunning.feedback") {
		var snapshot string
		err = tx.QueryRowContext(ctx, `SELECT cs.id FROM settlement_dunning_action da JOIN settlement_dunning_case dc ON dc.id=da.dunning_case_id AND dc.tenant_id=da.tenant_id LEFT JOIN settlement_receivable r ON r.id=dc.receivable_id AND r.tenant_id=dc.tenant_id LEFT JOIN settlement_receivable_plan rp ON rp.id=dc.plan_id AND rp.tenant_id=dc.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=COALESCE(r.contract_snapshot_id,rp.contract_snapshot_id) AND cs.tenant_id=dc.tenant_id WHERE da.tenant_id=? AND da.id=?`, p.TenantID, actionID).Scan(&snapshot)
		if err != nil {
			return ErrNotFound
		}
		if err = s.RequireContractAccessTx(ctx, tx, p, "settlement.dunning.feedback", snapshot); err != nil {
			return err
		}
	}
	if status == "COMPLETED" {
		return tx.Commit()
	}
	if status != "PENDING" || currentVersion != version {
		return ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE settlement_dunning_action SET status='COMPLETED',completed_at=UTC_TIMESTAMP(3),completed_by=?,result_detail=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='PENDING' AND version=?`, p.UserID, note, p.TenantID, actionID, version)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrConflict
	}
	if err := outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_DUNNING_ACTION_COMPLETED", "dunning_action", actionID, map[string]any{"actor_id": p.UserID, "result": "SUCCESS", "risk_level": "LOW", "note": note}); err != nil {
		return err
	}
	return tx.Commit()
}

type DunningCoveragePolicy struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Version       int    `json:"version"`
	AgingFromDays int    `json:"aging_from_days"`
	AgingToDays   int    `json:"aging_to_days"`
}

type DunningCoverageGap struct {
	FromDays  int  `json:"from_days"`
	ToDays    int  `json:"to_days,omitempty"`
	OpenEnded bool `json:"open_ended"`
}

type DunningCoverage struct {
	Policies []DunningCoveragePolicy `json:"policies"`
	Gaps     []DunningCoverageGap    `json:"gaps"`
}

// DunningCoverage 报告已启用策略的账龄覆盖缺口，不创建或
// 修改策略。扫描器绝不为缺口臆造接收者或规则；
// 必须由操作员决定业务负责人及动作。
