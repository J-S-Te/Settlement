package handover

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repository 实现：结算仓统一使用 database/sql（本仓不依赖 ORM）。
// 关键不变量：回执存在 ⇒ 责任变更已在同一事务内生效。
type SQLRepository struct{ db *sql.DB }

var _ Repository = (*SQLRepository)(nil)

func NewRepository(db *sql.DB) (*SQLRepository, error) {
	if db == nil {
		return nil, errors.New("handover repository requires a database")
	}
	return &SQLRepository{db: db}, nil
}

const (
	// 未完成催收动作的状态（扫描器写入时为 PENDING）。
	dunningActionPending = "PENDING"
	// settlement_dunning_action 没有 version/updated_at，无法做版本级并发检测；
	// 守卫退化为"仍为 PENDING 且收件人未变"，冲突原因码如下（已在证据中写明）。
	stateChangedReason = "SETTLEMENT_ACTION_NOT_PENDING"
)

// CreateFreeze 幂等创建冻结。
func (repository *SQLRepository) CreateFreeze(ctx context.Context, freeze Freeze) (Freeze, bool, error) {
	now := freeze.FrozenAt.UTC()
	result, err := repository.db.ExecContext(ctx,
		"INSERT IGNORE INTO settlement_handover_freeze(id, tenant_id, batch_id, request_id, user_id, status, frozen_at, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?)",
		freeze.ID, freeze.TenantID, freeze.BatchID, freeze.RequestID, freeze.UserID, freeze.Status, now, now, now)
	if err != nil {
		return Freeze{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Freeze{}, false, err
	}
	if affected == 0 {
		existing, found, lookupErr := repository.ActiveFreeze(ctx, freeze.TenantID, freeze.UserID)
		if lookupErr != nil {
			return Freeze{}, false, lookupErr
		}
		if !found {
			return Freeze{}, false, fmt.Errorf("handover freeze exists but is not active for %s", freeze.UserID)
		}
		return existing, false, nil
	}
	return freeze, true, nil
}

// ReleaseFreeze 解除冻结；重复解除幂等。
func (repository *SQLRepository) ReleaseFreeze(ctx context.Context, tenantID, batchID, outcome string, now time.Time) error {
	result, err := repository.db.ExecContext(ctx,
		"UPDATE settlement_handover_freeze SET status=?, released_at=?, outcome=?, updated_at=? WHERE tenant_id=? AND batch_id=? AND status=?",
		FreezeReleased, now.UTC(), outcome, now.UTC(), tenantID, batchID, FreezeFrozen)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		var count int
		if err := repository.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM settlement_handover_freeze WHERE tenant_id=? AND batch_id=?", tenantID, batchID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return ErrNotFound
		}
	}
	return nil
}

// ActiveFreeze 判断用户当前是否被交接冻结（写路径据此拒绝新责任）。
func (repository *SQLRepository) ActiveFreeze(ctx context.Context, tenantID, userID string) (Freeze, bool, error) {
	row := repository.db.QueryRowContext(ctx,
		"SELECT id, tenant_id, batch_id, request_id, user_id, status, frozen_at, released_at, outcome FROM settlement_handover_freeze WHERE tenant_id=? AND user_id=? AND status=? ORDER BY frozen_at DESC LIMIT 1",
		tenantID, userID, FreezeFrozen)
	freeze, err := scanFreeze(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Freeze{}, false, nil
	}
	if err != nil {
		return Freeze{}, false, err
	}
	return freeze, true, nil
}

// ListResponsibilities 返回该用户未完成的催收动作（status=PENDING）。
func (repository *SQLRepository) ListResponsibilities(ctx context.Context, tenantID, userID, cursor string, limit int) ([]Responsibility, error) {
	if limit <= 0 {
		limit = 100
	}
	query := "SELECT id, action_type, recipient_user_id, created_at FROM settlement_dunning_action WHERE tenant_id=? AND recipient_user_id=? AND status=?"
	args := []any{tenantID, userID, dunningActionPending}
	if strings.TrimSpace(cursor) != "" {
		query += " AND id > ?"
		args = append(args, strings.TrimSpace(cursor))
	}
	query += " ORDER BY id ASC LIMIT ?"
	args = append(args, limit)
	rows, err := repository.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Responsibility{}
	for rows.Next() {
		var id, actionType, recipient string
		var createdAt time.Time
		if err := rows.Scan(&id, &actionType, &recipient, &createdAt); err != nil {
			return nil, err
		}
		items = append(items, Responsibility{
			ResourceType: ResourceDunningAction, ResourceID: id, ResourceName: actionType,
			OwnerUserID: recipient, Category: CategoryDunningRecipient,
			// 该表无版本列：固定 1 表示"待处理"，真实守卫见 TransferOwnership。
			Version: 1, UpdatedAt: createdAt,
		})
	}
	return items, rows.Err()
}

// TransferOwnership 在本地事务内改派催收动作收件人并写入幂等回执。
func (repository *SQLRepository) TransferOwnership(ctx context.Context, in TransferInput, now time.Time) (Receipt, error) {
	if in.ResourceType != ResourceDunningAction {
		return Receipt{}, fmt.Errorf("unsupported resource type %s: %w", in.ResourceType, ErrValidation)
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback()

	if receipt, handled, err := existingReceipt(ctx, tx, in); err != nil || handled {
		return receipt, err
	}
	result, err := tx.ExecContext(ctx,
		"UPDATE settlement_dunning_action SET recipient_user_id=? WHERE tenant_id=? AND id=? AND recipient_user_id=? AND status=?",
		in.ToUserID, in.TenantID, in.ResourceID, in.FromUserID, dunningActionPending)
	if err != nil {
		return Receipt{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Receipt{}, err
	}
	status, reason, evidence := ReceiptApplied, "", "settlement:dunning_action:"+in.ResourceID
	if affected != 1 {
		status, reason, evidence = ReceiptRejected, stateChangedReason, ""
	}
	receipt, err := writeReceipt(ctx, tx, in, now, status, reason, evidence)
	if err != nil {
		return Receipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

// FindReceipt 按幂等键查询既有回执。
func (repository *SQLRepository) FindReceipt(ctx context.Context, tenantID, idempotencyKey string) (Receipt, bool, error) {
	row := repository.db.QueryRowContext(ctx,
		"SELECT item_id, idempotency_key, status, applied_version, reason_code, evidence_ref, received_at FROM settlement_handover_receipt WHERE tenant_id=? AND idempotency_key=?",
		tenantID, idempotencyKey)
	receipt, err := scanReceipt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, err
	}
	return receipt, true, nil
}

func existingReceipt(ctx context.Context, tx *sql.Tx, in TransferInput) (Receipt, bool, error) {
	row := tx.QueryRowContext(ctx,
		"SELECT item_id, idempotency_key, status, applied_version, reason_code, evidence_ref, received_at FROM settlement_handover_receipt WHERE tenant_id=? AND idempotency_key=?",
		in.TenantID, in.IdempotencyKey)
	receipt, err := scanReceipt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, err
	}
	return receipt, true, nil
}

// writeReceipt 写入回执（REJECTED 同样落库，便于平台核对）；同键重复写入按幂等处理。
func writeReceipt(ctx context.Context, tx *sql.Tx, in TransferInput, now time.Time, status, reason, evidence string) (Receipt, error) {
	now = now.UTC()
	_, err := tx.ExecContext(ctx,
		"INSERT INTO settlement_handover_receipt(id, tenant_id, batch_id, item_id, resource_type, resource_id, from_user_id, to_user_id, idempotency_key, status, applied_version, reason_code, evidence_ref, received_at, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		shortReceiptID(in.IdempotencyKey), in.TenantID, in.BatchID, in.ItemID, in.ResourceType, in.ResourceID,
		in.FromUserID, in.ToUserID, in.IdempotencyKey, status, appliedVersion(status), nullable(reason), nullable(evidence), now, now)
	if err != nil {
		if isDuplicate(err) {
			// 并发重放：返回既有回执。
			row := tx.QueryRowContext(context.WithoutCancel(ctx),
				"SELECT item_id, idempotency_key, status, applied_version, reason_code, evidence_ref, received_at FROM settlement_handover_receipt WHERE tenant_id=? AND idempotency_key=?",
				in.TenantID, in.IdempotencyKey)
			if receipt, scanErr := scanReceipt(row); scanErr == nil {
				return receipt, nil
			}
		}
		return Receipt{}, err
	}
	return Receipt{ItemID: in.ItemID, IdempotencyKey: in.IdempotencyKey, Status: status,
		AppliedVersion: appliedVersion(status), ReasonCode: reason, EvidenceRef: evidence, ReceivedAt: now}, nil
}

func appliedVersion(status string) uint64 {
	if status == ReceiptApplied {
		return 1
	}
	return 0
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.TrimSpace(value)
}

type rowScanner interface{ Scan(...any) error }

func scanFreeze(row rowScanner) (Freeze, error) {
	var freeze Freeze
	var releasedAt sql.NullTime
	var outcome sql.NullString
	if err := row.Scan(&freeze.ID, &freeze.TenantID, &freeze.BatchID, &freeze.RequestID, &freeze.UserID,
		&freeze.Status, &freeze.FrozenAt, &releasedAt, &outcome); err != nil {
		return Freeze{}, err
	}
	if releasedAt.Valid {
		freeze.ReleasedAt = &releasedAt.Time
	}
	if outcome.Valid {
		freeze.Outcome = outcome.String
	}
	return freeze, nil
}

func scanReceipt(row rowScanner) (Receipt, error) {
	var receipt Receipt
	var reason, evidence sql.NullString
	if err := row.Scan(&receipt.ItemID, &receipt.IdempotencyKey, &receipt.Status, &receipt.AppliedVersion,
		&reason, &evidence, &receipt.ReceivedAt); err != nil {
		return Receipt{}, err
	}
	if reason.Valid {
		receipt.ReasonCode = reason.String
	}
	if evidence.Valid {
		receipt.EvidenceRef = evidence.String
	}
	return receipt, nil
}

// shortReceiptID 生成 32 字符回执主键（结算仓约定：ID 必须 32 字符）。
func shortReceiptID(idempotencyKey string) string {
	sum := 0
	for _, r := range idempotencyKey {
		sum = (sum*31 + int(r)) % 100000000
	}
	return "01STL" + fmt.Sprintf("%027d", sum)
}

func isDuplicate(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate")
}
