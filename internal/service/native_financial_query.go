package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// nativePage 校验所有本地目录查询的明确边界。
// 参数：limit 为每页条数，offset 为偏移。
// 返回值：越界时返回输入错误。
func nativePage(limit, offset int) error {
	if limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return ErrInvalid
	}
	return nil
}

// ListNativeFinancialCustomers 返回本租户可用的本地客户，不跨源猜测客户身份。
// 参数：ctx/p 为授权上下文，limit/offset 为页范围。
// 返回值：有界客户目录；未启用、无权限或存储错误时失败。
func (s *Service) ListNativeFinancialCustomers(ctx context.Context, p Principal, limit, offset int) ([]NativeFinancialCustomer, error) {
	if err := s.requireNativeFinancial(p, false); err != nil {
		return nil, err
	}
	if err := nativePage(limit, offset); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name FROM settlement_financial_customer WHERE tenant_id=? AND status='ACTIVE' ORDER BY name,id LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []NativeFinancialCustomer{}
	for rows.Next() {
		var item NativeFinancialCustomer
		if err = rows.Scan(&item.ID, &item.Name); err != nil {
			return nil, err
		}
		item.CustomerKey, item.Namespace = NativeCustomerPrefix+item.ID, "settlement_local"
		items = append(items, item)
	}
	return items, rows.Err()
}

const nativeBaselineSelect = `SELECT id,basis_type,local_document_id,local_document_no,local_document_version,customer_id,customer_name_snapshot,currency,contract_amount,DATE_FORMAT(CONVERT_TZ(effective_at,'+00:00','+08:00'),'%Y-%m-%d'),financial_status,COALESCE(local_evidence_type,''),COALESCE(local_evidence_reference,''),COALESCE(local_evidence_note,''),source_contract_id,source_contract_no,customer_namespace FROM settlement_contract_snapshot WHERE tenant_id=? AND basis_type='LOCAL_FINANCIAL'`

type nativeRowScanner interface{ Scan(...any) error }

// scanNativeBaseline 使用可空类型接收真实缺失的外部合同ID。
// 参数：row 为已限定本租户与本地来源的查询结果。
// 返回值：本地财务基线；NULL不会变成伪造的外部标识。
func scanNativeBaseline(row nativeRowScanner) (NativeFinancialBaseline, error) {
	var item NativeFinancialBaseline
	var contractID sql.NullString
	err := row.Scan(&item.ID, &item.BasisType, &item.LocalDocumentID, &item.LocalDocumentNo, &item.Version, &item.CustomerKey, &item.CustomerName, &item.Currency, &item.Amount, &item.EffectiveDate, &item.Status, &item.EvidenceType, &item.EvidenceReference, &item.EvidenceNote, &contractID, &item.SourceContractNo, &item.CustomerNamespace)
	if contractID.Valid {
		item.SourceContractID = &contractID.String
	}
	return item, err
}

// GetNativeFinancialBaseline 返回真实本地依据，并保留明确来源元数据。
// 参数：ctx/p 为授权上下文，id 为结算内部基线ID。
// 返回值：本地基线；越租户、合同来源或不存在均返回不存在。
func (s *Service) GetNativeFinancialBaseline(ctx context.Context, p Principal, id string) (NativeFinancialBaseline, error) {
	if err := s.requireNativeFinancial(p, false); err != nil {
		return NativeFinancialBaseline{}, err
	}
	return s.getNativeFinancialBaseline(ctx, p.TenantID, id)
}

// getNativeFinancialBaseline 供已授权读取和已通过创建幂等校验的命令返回真实现态。
// 参数：ctx 为请求上下文，tenant/id 为已验证的范围及内部ID。
// 返回值：本地基线；不额外扩大列表或跨租户读取能力。
func (s *Service) getNativeFinancialBaseline(ctx context.Context, tenant, id string) (NativeFinancialBaseline, error) {
	item, err := scanNativeBaseline(s.DB.QueryRowContext(ctx, nativeBaselineSelect+` AND id=?`, tenant, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return item, err
}

// ListNativeFinancialBaselines 返回有界本地财务依据历史，包括已作废依据。
// 参数：ctx/p 为授权上下文，limit/offset 为页范围。
// 返回值：本地基线列表；保留空合同ID及实际财务状态。
func (s *Service) ListNativeFinancialBaselines(ctx context.Context, p Principal, limit, offset int) ([]NativeFinancialBaseline, error) {
	if err := s.requireNativeFinancial(p, false); err != nil {
		return nil, err
	}
	if err := nativePage(limit, offset); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, nativeBaselineSelect+` ORDER BY created_at DESC,id LIMIT ? OFFSET ?`, p.TenantID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []NativeFinancialBaseline{}
	for rows.Next() {
		item, e := scanNativeBaseline(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// VoidNativeFinancialBaseline 仅作废尚未形成正式账务的本地依据及未确认计划。
// 参数：ctx/p 为授权上下文，id 为内部ID，key 为幂等键，in 为版本及原因。
// 返回值：成功无错误；已确认、已占用或源不符时保持原事实不变。
func (s *Service) VoidNativeFinancialBaseline(ctx context.Context, p Principal, id, key string, in NativeFinancialVoidInput) error {
	if err := s.requireNativeFinancial(p, true); err != nil {
		return err
	}
	key, err := nativeIdempotencyKey(key)
	if err != nil {
		return err
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if id == "" || in.Version < 1 || in.Reason == "" || textTooLong(in.Reason, 500) {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, replay, err := claimIdempotency(ctx, tx, p.TenantID, "VOID_NATIVE_FINANCIAL", key, id, struct {
		Actor, ID string
		Input     NativeFinancialVoidInput
	}{p.UserID, id, in})
	if err != nil {
		return err
	}
	if replay {
		return tx.Commit()
	}
	var status string
	var version int
	err = tx.QueryRowContext(ctx, `SELECT financial_status,local_document_version FROM settlement_contract_snapshot WHERE tenant_id=? AND id=? AND basis_type='LOCAL_FINANCIAL' FOR UPDATE`, p.TenantID, id).Scan(&status, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "EFFECTIVE" || version != in.Version {
		return ErrConflict
	}
	if err = lockNativeUnconsumed(ctx, tx, p.TenantID, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE settlement_contract_snapshot SET financial_status='VOID',local_document_version=local_document_version+1,local_void_reason=?,local_voided_by=?,local_voided_at=UTC_TIMESTAMP(3),updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND basis_type='LOCAL_FINANCIAL' AND local_document_version=? AND financial_status='EFFECTIVE'`, in.Reason, p.UserID, p.TenantID, id, in.Version)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable_plan SET confirmation_status='CANCELLED',confirmation_reason=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND contract_snapshot_id=? AND confirmation_status='PENDING_CONFIRMATION'`, in.Reason, p.TenantID, id); err != nil {
		return err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_NATIVE_FINANCIAL_VOIDED", "financial_baseline", id, map[string]any{"actor_id": p.UserID, "reason": in.Reason, "old_version": version, "new_version": version + 1, "result": "SUCCESS", "risk_level": "HIGH"}); err != nil {
		return err
	}
	return tx.Commit()
}

// lockNativeUnconsumed 在当前锁定读中核验本地依据不存在正式应收或开票事实。
// 参数：ctx/tx 为当前事务，tenant/snapshot 为可信范围。
// 返回值：任一计划已确认或已产生子事实时拒绝，绝不删除财务记录。
func lockNativeUnconsumed(ctx context.Context, tx *sql.Tx, tenant, snapshot string) error {
	rows, err := tx.QueryContext(ctx, `SELECT confirmation_status FROM settlement_receivable_plan WHERE tenant_id=? AND contract_snapshot_id=? ORDER BY id FOR UPDATE`, tenant, snapshot)
	if err != nil {
		return err
	}
	for rows.Next() {
		var state string
		if err = rows.Scan(&state); err != nil {
			rows.Close()
			return err
		}
		if state != "PENDING_CONFIRMATION" {
			rows.Close()
			return ErrNativeFinancialConsumed
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, table := range []string{"settlement_receivable", "settlement_invoice_request"} {
		var id string
		err = tx.QueryRowContext(ctx, `SELECT id FROM `+table+` WHERE tenant_id=? AND contract_snapshot_id=? LIMIT 1 FOR UPDATE`, tenant, snapshot).Scan(&id)
		if err == nil {
			return ErrNativeFinancialConsumed
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	var dunning string
	err = tx.QueryRowContext(ctx, `SELECT dc.id FROM settlement_dunning_case dc JOIN settlement_receivable_plan rp ON rp.id=dc.plan_id AND rp.tenant_id=dc.tenant_id WHERE rp.tenant_id=? AND rp.contract_snapshot_id=? LIMIT 1 FOR UPDATE`, tenant, snapshot).Scan(&dunning)
	if err == nil {
		return ErrNativeFinancialConsumed
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect local dunning facts: %w", err)
	}
	return nil
}
