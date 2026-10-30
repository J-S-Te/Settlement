package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
)

type ImportReconcileInput struct {
	Currency               string `json:"currency"`
	ExpectedOpenAmount     string `json:"expected_open_amount"`
	ExpectedReceiptAmount  string `json:"expected_receipt_amount"`
	ExpectedInvoicedAmount string `json:"expected_invoiced_amount"`
	CreditDecision         string `json:"credit_decision"`
	Reason                 string `json:"reason"`
}
type ImportReconciliation struct {
	ID                 string `json:"reconciliation_id"`
	ContractSnapshotID string `json:"contract_snapshot_id"`
	SnapshotVersion    uint64 `json:"snapshot_version"`
	State              string `json:"state"`
	Currency           string `json:"currency"`
	ContractAmount     string `json:"contract_amount"`
	PlannedAmount      string `json:"planned_amount"`
	ConfirmedAmount    string `json:"confirmed_amount"`
	AllocatedAmount    string `json:"allocated_amount"`
	OpenAmount         string `json:"open_amount"`
	ReceiptAmount      string `json:"receipt_amount"`
	InvoicedAmount     string `json:"invoiced_amount"`
	FactCount          int    `json:"fact_count"`
}

// importBalances 在共用快照锁内计算本地逐币种余额并验证完整原分期及逐事实清单。
// 参数：ctx为上下文，tx为读写事务，tenant为可信租户，id为财务快照。
// 返回值：真实余额、财务模式及异常；任何清单缺口阻止对账。
func importBalances(ctx context.Context, tx *sql.Tx, tenant, id string) (ImportReconciliation, string, error) {
	b := ImportReconciliation{ContractSnapshotID: id, State: "READY"}
	var mode string
	var expected []byte
	err := tx.QueryRowContext(ctx, `SELECT cs.import_snapshot_version,cs.currency,cs.contract_amount,ci.import_mode,ci.expected_facts_json FROM settlement_contract_snapshot cs JOIN settlement_contract_import ci ON ci.contract_snapshot_id=cs.id AND ci.tenant_id=cs.tenant_id WHERE cs.tenant_id=? AND cs.id=? AND cs.migration_state='HOLD' FOR UPDATE`, tenant, id).Scan(&b.SnapshotVersion, &b.Currency, &b.ContractAmount, &mode, &expected)
	if err != nil {
		return b, mode, err
	}
	var pending int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(planned_amount),0),SUM(confirmation_status<>'CONFIRMED') FROM settlement_receivable_plan WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, id).Scan(&b.PlannedAmount, &pending)
	if err != nil {
		return b, mode, err
	}
	if pending != 0 || add(b.PlannedAmount).Cmp(add(b.ContractAmount)) != 0 {
		return b, mode, ErrConflict
	}
	var writeoff string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(original_amount),0),COALESCE(SUM(received_allocated_amount),0),COALESCE(SUM(open_amount),0),COALESCE(SUM(invoiced_amount),0),COALESCE(SUM(write_off_amount),0) FROM settlement_receivable WHERE tenant_id=? AND contract_snapshot_id=? AND currency=?`, tenant, id, b.Currency).Scan(&b.ConfirmedAmount, &b.AllocatedAmount, &b.OpenAmount, &b.InvoicedAmount, &writeoff)
	if err != nil {
		return b, mode, err
	}
	if add(b.ConfirmedAmount).Cmp(add(b.ContractAmount)) != 0 || add(b.OpenAmount, b.AllocatedAmount, writeoff).Cmp(add(b.ConfirmedAmount)) != 0 {
		return b, mode, ErrConflict
	}
	var actualAllocated string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(a.allocated_amount-COALESCE((SELECT SUM(ar.reversed_amount) FROM settlement_receipt_allocation_reversal ar WHERE ar.tenant_id=a.tenant_id AND ar.original_allocation_id=a.id),0)),0) FROM settlement_receipt_allocation a JOIN settlement_receivable r ON r.id=a.receivable_id AND r.tenant_id=a.tenant_id WHERE r.tenant_id=? AND r.contract_snapshot_id=?`, tenant, id).Scan(&actualAllocated)
	if err != nil {
		return b, mode, err
	}
	if add(actualAllocated).Cmp(add(b.AllocatedAmount)) != 0 {
		return b, mode, ErrConflict
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(r.amount),0) FROM settlement_import_fact f JOIN settlement_receipt r ON r.id=f.target_business_id AND r.tenant_id=f.tenant_id WHERE f.tenant_id=? AND f.contract_snapshot_id=? AND f.fact_type='RECEIPT'`, tenant, id).Scan(&b.ReceiptAmount)
	if err != nil {
		return b, mode, err
	}
	var facts []ExpectedImportFact
	if err = json.Unmarshal(expected, &facts); err != nil {
		return b, mode, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_system,fact_type,external_fact_id FROM settlement_import_fact WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, id)
	if err != nil {
		return b, mode, err
	}
	restored := map[ExpectedImportFact]bool{}
	for rows.Next() {
		var f ExpectedImportFact
		if err = rows.Scan(&f.SourceSystem, &f.FactType, &f.ExternalFactID); err != nil {
			rows.Close()
			return b, mode, err
		}
		restored[f] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return b, mode, err
	}
	b.FactCount = len(restored)
	if b.FactCount != len(facts) {
		return b, mode, ErrImportBlocked
	}
	for _, f := range facts {
		if !restored[f] {
			return b, mode, ErrImportBlocked
		}
	}
	if mode == "HISTORICAL_CLOSED" && add(b.OpenAmount).Sign() != 0 {
		return b, mode, ErrConflict
	}
	// 历史票据尚无安全恢复适配器；任何票额均不可在本阶段放行。
	if add(b.InvoicedAmount).Sign() != 0 {
		return b, mode, ErrImportBlocked
	}
	return b, mode, nil
}

// ReconcileContractImport 财务核对批准来源的期初余额，并持久化当前快照的放行凭证。
// 参数：ctx为上下文，p为有财务范围的核对审批人，id为快照，in为批准来源金额和信用选择。
// 返回值：本地reconciliation_id及版本；差异、未恢复事实或无权限时拒绝。
func (s *Service) ReconcileContractImport(ctx context.Context, p Principal, id string, in ImportReconcileInput) (ImportReconciliation, error) {
	var result ImportReconciliation
	if err := financeAccess(p, "settlement.import.reconcile"); err != nil {
		return result, err
	}
	if err := financeAccess(p, "settlement.import.release"); err != nil {
		return result, err
	}
	if in.CreditDecision != "PUBLISH" && in.CreditDecision != "SUPPRESSED" || in.Reason == "" || textTooLong(in.Reason, 500) {
		return result, ErrInvalid
	}
	for _, v := range []string{in.ExpectedOpenAmount, in.ExpectedReceiptAmount, in.ExpectedInvoicedAmount} {
		if _, err := nonNegativeAmount(v); err != nil {
			return result, err
		}
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	result, _, err = importBalances(ctx, tx, p.TenantID, id)
	if err != nil {
		return result, err
	}
	if in.Currency != result.Currency || add(in.ExpectedOpenAmount).Cmp(add(result.OpenAmount)) != 0 || add(in.ExpectedReceiptAmount).Cmp(add(result.ReceiptAmount)) != 0 || add(in.ExpectedInvoicedAmount).Cmp(add(result.InvoicedAmount)) != 0 {
		return result, ErrConflict
	}
	payload, _ := json.Marshal(result)
	hash := sha256.Sum256(payload)
	result.ID = newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_import_reconciliation(id,tenant_id,contract_snapshot_id,snapshot_version,balances_hash,statement_json,reconciled_by,created_at) VALUES(?,?,?,?,?,?,?,UTC_TIMESTAMP(3))`, result.ID, p.TenantID, id, result.SnapshotVersion, hash[:], payload, p.UserID)
	if err != nil {
		return result, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_contract_import SET state='READY',credit_decision=?,credit_reason=?,approved_by=?,reconciliation_id=? WHERE tenant_id=? AND contract_snapshot_id=?`, in.CreditDecision, in.Reason, p.UserID, result.ID, p.TenantID, id)
	if err != nil {
		return result, err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_IMPORT_RECONCILED", "contract_snapshot", id, map[string]any{"actor_id": p.UserID, "reconciliation_id": result.ID, "snapshot_version": result.SnapshotVersion, "credit_decision": in.CreditDecision, "reason": in.Reason}); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// ReleaseContractImport 用行锁复核本地凭证、事实版本和权威责任后独立放行。
// 参数：ctx为上下文，tenant为验签租户，id为财务快照，in为稳定RELEASE命令。
// 返回值：仅真正放行返回FINANCE_RELEASED；旧对账、责任冲突及异内容重放均拒绝。
func (s *Service) ReleaseContractImport(ctx context.Context, tenant, id string, in ContractImportCommand) (ImportReceipt, error) {
	result := ImportReceipt{EventID: in.EventID, ContractSnapshotID: id, Status: "FINANCE_RELEASED", MigrationState: "RELEASED"}
	if err := validateImportCommand(tenant, "RELEASE", in); err != nil {
		return result, err
	}
	if in.ReconciliationID == "" || in.ExpectedSnapshotVersion == 0 {
		return result, ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var source, owner, mode, state, decision, reason, approver, item, digest, currentReconciliation string
	var revision, release uint64
	err = tx.QueryRowContext(ctx, `SELECT cs.source_contract_id,cs.migration_state,ci.owner_user_id,ci.import_mode,ci.credit_decision,ci.credit_reason,ci.approved_by,ci.origin_import_item_id,ci.source_revision,ci.policy_digest,ci.release_version,ci.reconciliation_id FROM settlement_contract_snapshot cs JOIN settlement_contract_import ci ON ci.contract_snapshot_id=cs.id AND ci.tenant_id=cs.tenant_id WHERE cs.tenant_id=? AND cs.id=? FOR UPDATE`, tenant, id).Scan(&source, &state, &owner, &mode, &decision, &reason, &approver, &item, &revision, &digest, &release, &currentReconciliation)
	if err != nil {
		return result, err
	}
	if item != in.OriginImportItemID || revision != in.SourceRevision || digest != in.PolicyDigest || release != in.ReleaseVersion || mode != in.ImportMode {
		return result, ErrConflict
	}
	_, replay, err := claimIdempotency(ctx, tx, tenant, "IMPORT_RELEASE", in.EventID, id, in)
	if err != nil {
		return result, err
	}
	if replay {
		if state != "RELEASED" {
			return result, ErrConflict
		}
		result.Duplicate = true
		return result, nil
	}
	if currentReconciliation != in.ReconciliationID || state != "HOLD" || reason == "" || approver == "" || (decision != "PUBLISH" && decision != "SUPPRESSED") {
		return result, ErrConflict
	}
	var hash []byte
	var version uint64
	err = tx.QueryRowContext(ctx, `SELECT balances_hash,snapshot_version FROM settlement_import_reconciliation WHERE tenant_id=? AND contract_snapshot_id=? AND id=? FOR UPDATE`, tenant, id, in.ReconciliationID).Scan(&hash, &version)
	if err != nil {
		return result, ErrConflict
	}
	balances, _, err := importBalances(ctx, tx, tenant, id)
	if err != nil {
		return result, err
	}
	payload, _ := json.Marshal(balances)
	current := sha256.Sum256(payload)
	if version != balances.SnapshotVersion || version != in.ExpectedSnapshotVersion || !bytes.Equal(hash, current[:]) {
		return result, ErrConflict
	}
	if mode == "CONTINUE_LIVE" {
		var active bool
		var actualOwner string
		var actualVersion uint64
		err = tx.QueryRowContext(ctx, `SELECT owner_user_id,version,active FROM settlement_contract_responsibility WHERE tenant_id=? AND source_contract_id=? FOR SHARE`, tenant, source).Scan(&actualOwner, &actualVersion, &active)
		if err != nil || !active || actualOwner != owner || actualVersion != in.ExpectedOwnerVersion {
			return result, fmt.Errorf("%w: responsibility projection does not match", ErrConflict)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_contract_snapshot SET migration_state='RELEASED' WHERE tenant_id=? AND id=?`, tenant, id); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_contract_import SET state='RELEASED',released_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, id); err != nil {
		return result, err
	}
	status := "PENDING"
	if decision == "SUPPRESSED" {
		status = "SUPPRESSED"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_credit_sync c JOIN settlement_receivable r ON r.id=c.receivable_id AND r.tenant_id=c.tenant_id SET c.status=?,c.updated_at=UTC_TIMESTAMP(3) WHERE r.tenant_id=? AND r.contract_snapshot_id=? AND c.status IN ('HOLD','PENDING','RETRY_WAIT','PROCESSING')`, status, tenant, id); err != nil {
		return result, err
	}
	if err = outbox(ctx, tx, tenant, "PLATFORM_AUDIT", "SETTLEMENT_IMPORT_RELEASED", "contract_snapshot", id, map[string]any{"actor_id": approver, "credit_decision": decision, "reason": reason, "reconciliation_id": in.ReconciliationID, "snapshot_version": version}); err != nil {
		return result, err
	}
	return result, tx.Commit()
}
