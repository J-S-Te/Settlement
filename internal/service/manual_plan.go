package service

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type ManualPlanInput struct {
	ContractSnapshotID string `json:"contract_snapshot_id"`
	InstallmentNo      int    `json:"installment_no"`
	DueDate            string `json:"due_date"`
	PlannedAmount      string `json:"planned_amount"`
	Reason             string `json:"reason"`
}

// CreateManualPlan 用途：在合同金额约束内创建幂等人工待确认计划。
// 参数：ctx为上下文，p为主体，key为幂等键，in为绑定合同的计划。
// 返回值：返回计划ID，校验、授权、并发或存储失败返回错误。
func (s *Service) CreateManualPlan(ctx context.Context, p Principal, key string, in ManualPlanInput) (string, error) {
	if !p.Allows("settlement.receivable.create") {
		return "", ErrForbidden
	}
	if strings.TrimSpace(key) == "" || len(key) > 128 || in.InstallmentNo < 1 || strings.TrimSpace(in.Reason) == "" || textTooLong(in.Reason, 500) {
		return "", ErrInvalid
	}
	if _, err := time.Parse("2006-01-02", in.DueDate); err != nil {
		return "", ErrInvalid
	}
	planned, err := amount(in.PlannedAmount)
	if err != nil {
		return "", err
	}
	in.PlannedAmount = planned
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = s.RequireContractAccessTx(ctx, tx, p, "settlement.receivable.create", in.ContractSnapshotID); err != nil {
		return "", err
	}
	id := newID()
	existing, replay, err := claimIdempotency(ctx, tx, p.TenantID, "CREATE_MANUAL_PLAN", key, id, in)
	if err != nil {
		return "", err
	}
	if replay {
		return existing, nil
	}
	if err = checkPlanBudget(ctx, tx, p.TenantID, in.ContractSnapshotID, "", planned); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,confirmation_reason,created_at,updated_at) VALUES(?,?,?,?,?,?,'MANUAL','PENDING_CONFIRMATION',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, in.ContractSnapshotID, in.InstallmentNo, in.DueDate, planned, in.Reason)
	if err != nil {
		if mysqlDuplicate(err) {
			return "", ErrConflict
		}
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_MANUAL_PLAN_CREATED", "receivable_plan", id, map[string]any{"actor_id": p.UserID, "reason": in.Reason, "amount": planned, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// checkPlanBudget 用途：串行检查合同下计划合计不超过合同金额。
// 参数：ctx为上下文，tx为事务，tenant为租户，snapshot为合同，excluded为被替换计划，planned为新金额。
// 返回值：超额返回冲突，合同不存在或失效返回不存在，其余返回存储错误。
func checkPlanBudget(ctx context.Context, tx *sql.Tx, tenant, snapshot, excluded, planned string) error {
	var cap, total string
	if err := tx.QueryRowContext(ctx, `SELECT contract_amount FROM settlement_contract_snapshot WHERE tenant_id=? AND id=? AND financial_status='EFFECTIVE' FOR UPDATE`, tenant, snapshot).Scan(&cap); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(planned_amount),0) FROM settlement_receivable_plan WHERE tenant_id=? AND contract_snapshot_id=? AND id<>?`, tenant, snapshot, excluded).Scan(&total); err != nil {
		return err
	}
	if decimalGreater(add(total, planned).FloatString(2), cap) {
		return ErrConflict
	}
	return nil
}

// AdjustPlan 用途：调整已确认计划并同步应收，保留已开票及核销金额下限。
// 参数：ctx为上下文，p为主体，id为计划，in为期望版本和新值及原因。
// 返回值：成功无错误；余额、版本、权限或存储不满足时返回错误。
func (s *Service) AdjustPlan(ctx context.Context, p Principal, id string, in PlanConfirmationInput) error {
	if !p.Allows("settlement.receivable.adjust") {
		return ErrForbidden
	}
	if in.Version < 1 || strings.TrimSpace(in.Reason) == "" || textTooLong(in.Reason, 500) {
		return ErrInvalid
	}
	if _, err := time.Parse("2006-01-02", in.DueDate); err != nil {
		return ErrInvalid
	}
	planned, err := amount(in.PlannedAmount)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var snapshot, oldDate, oldAmount, receivable, used, original string
	err = tx.QueryRowContext(ctx, `SELECT rp.contract_snapshot_id,DATE_FORMAT(rp.due_date,'%Y-%m-%d'),rp.planned_amount,r.id,r.received_allocated_amount+r.write_off_amount,r.original_amount FROM settlement_receivable_plan rp JOIN settlement_receivable r ON r.receivable_plan_id=rp.id AND r.tenant_id=rp.tenant_id WHERE rp.tenant_id=? AND rp.id=? AND rp.version=? AND rp.confirmation_status='CONFIRMED' FOR UPDATE`, p.TenantID, id, in.Version).Scan(&snapshot, &oldDate, &oldAmount, &receivable, &used, &original)
	if err != nil {
		if err == sql.ErrNoRows {
			return ErrConflict
		}
		return err
	}
	if err = s.RequireContractAccessTx(ctx, tx, p, "settlement.receivable.adjust", snapshot); err != nil {
		return err
	}
	if decimalGreater(used, planned) {
		return ErrConflict
	}
	var invoiced string
	if err = tx.QueryRowContext(ctx, `SELECT invoiced_amount+COALESCE((SELECT SUM(reserved_amount-invoiced_amount-released_amount) FROM settlement_invoice_request_allocation a WHERE a.tenant_id=r.tenant_id AND a.receivable_id=r.id AND a.status='RESERVED'),0) FROM settlement_receivable r WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, receivable).Scan(&invoiced); err != nil {
		return err
	}
	if decimalGreater(invoiced, planned) {
		return ErrConflict
	}
	if err = checkPlanBudget(ctx, tx, p.TenantID, snapshot, id, planned); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable_plan SET due_date=?,planned_amount=?,confirmation_reason=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND version=?`, in.DueDate, planned, in.Reason, p.TenantID, id, in.Version); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET due_date=?,original_amount=?,open_amount=?-received_allocated_amount-write_off_amount,collection_status=CASE WHEN ?=received_allocated_amount+write_off_amount THEN 'SETTLED' WHEN received_allocated_amount>0 THEN 'PARTIALLY_SETTLED' ELSE 'UNPAID' END,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=?`, in.DueDate, planned, planned, planned, p.TenantID, receivable); err != nil {
		return err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_PLAN_ADJUSTED", "receivable_plan", id, map[string]any{"actor_id": p.UserID, "old_due_date": oldDate, "due_date": in.DueDate, "old_amount": oldAmount, "amount": planned, "reason": in.Reason, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateProjectMappingNote 用途：在本人协作范围内维护映射说明，不改变合同客户权威归属。
// 参数：ctx为上下文，p为主体，project为项目，version为分配版本，note为映射说明。
// 返回值：无权限或版本冲突返回错误，成功时无错误。
func (s *Service) UpdateProjectMappingNote(ctx context.Context, p Principal, project string, version uint64, note string) error {
	if !p.Allows("settlement.mapping.manage") {
		return ErrForbidden
	}
	if project == "" || version == 0 || textTooLong(note, 500) {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE settlement_project_assignment pa JOIN settlement_project_reference pr ON pr.tenant_id=pa.tenant_id AND pr.project_id=pa.project_id AND pr.active=TRUE SET pa.note=?,pa.version=pa.version+1,pa.updated_by=?,pa.updated_at=UTC_TIMESTAMP(3) WHERE pa.tenant_id=? AND pa.project_id=? AND pa.user_id=? AND pa.active=TRUE AND pa.version=?`, note, p.UserID, p.TenantID, project, p.UserID, version)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_PROJECT_MAPPING_NOTED", "project", project, map[string]any{"actor_id": p.UserID, "note": note, "result": "SUCCESS", "risk_level": "LOW"}); err != nil {
		return err
	}
	return tx.Commit()
}
