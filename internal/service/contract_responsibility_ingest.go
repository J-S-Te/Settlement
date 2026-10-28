package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ContractResponsibilityEvent 是合同权威端独立于财务事实的责任流。
type ContractResponsibilityEvent struct {
	EventID          string    `json:"event_id"`
	EventType        string    `json:"event_type"`
	TenantID         string    `json:"tenant_id"`
	OccurredAt       time.Time `json:"occurred_at"`
	SourceContractID string    `json:"source_contract_id"`
	SourceVersion    uint64    `json:"source_version"`
	OwnerUserID      string    `json:"owner_user_id"`
	Active           bool      `json:"active"`
}

// validateContractResponsibility 校验可信租户与责任事件边界。
// 参数：trustedTenant 为验签租户，event 为合同责任变更。
// 返回值：不合法时返回 ErrInvalid，合法返回 nil。
func validateContractResponsibility(trustedTenant string, event ContractResponsibilityEvent) error {
	if strings.TrimSpace(trustedTenant) == "" || event.TenantID != trustedTenant {
		return fmt.Errorf("%w: tenant_id must equal the verified machine tenant", ErrInvalid)
	}
	if event.EventType != "contract.responsibility_changed.v1" || event.SourceVersion == 0 || event.OccurredAt.IsZero() {
		return fmt.Errorf("%w: responsibility event type, version and occurred_at are required", ErrInvalid)
	}
	for _, field := range []struct {
		value string
		max   int
	}{{event.EventID, 128}, {event.SourceContractID, 128}, {event.TenantID, 64}} {
		if strings.TrimSpace(field.value) == "" || field.value != strings.TrimSpace(field.value) || textTooLong(field.value, field.max) {
			return fmt.Errorf("%w: invalid responsibility identifier", ErrInvalid)
		}
	}
	if textTooLong(event.OwnerUserID, 128) || event.OwnerUserID != strings.TrimSpace(event.OwnerUserID) || event.Active && event.OwnerUserID == "" {
		return fmt.Errorf("%w: active responsibility requires a stable owner_user_id", ErrInvalid)
	}
	return nil
}

// IngestContractResponsibility 原子接收责任事件，拒绝幂等冲突并阻止旧版本恢复权限。
// 参数：ctx 控制事务，trustedTenant 为机器可信租户，event 为独立责任流。
// 返回值：仅应用更新返回 true；重复或迟到返回 false；校验、冲突或存储失败返回错误。
func (s *Service) IngestContractResponsibility(ctx context.Context, trustedTenant string, event ContractResponsibilityEvent) (bool, error) {
	if err := validateContractResponsibility(trustedTenant, event); err != nil {
		return false, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256(payload)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_contract_responsibility(tenant_id,source_contract_id,owner_user_id,version,active,updated_at) VALUES(?,?, '',0,FALSE,UTC_TIMESTAMP(3)) ON DUPLICATE KEY UPDATE version=version`, event.TenantID, event.SourceContractID); err != nil {
		return false, err
	}
	var owner string
	var version uint64
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT owner_user_id,version,active FROM settlement_contract_responsibility WHERE tenant_id=? AND source_contract_id=? FOR UPDATE`, event.TenantID, event.SourceContractID).Scan(&owner, &version, &active); err != nil {
		return false, err
	}
	var prior []byte
	err = tx.QueryRowContext(ctx, `SELECT payload_hash FROM settlement_contract_responsibility_inbox WHERE tenant_id=? AND event_id=?`, event.TenantID, event.EventID).Scan(&prior)
	if err == nil {
		if !bytes.Equal(prior, hash[:]) {
			return false, ErrConflict
		}
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	if event.SourceVersion == version && (owner != event.OwnerUserID || active != event.Active) {
		return false, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_contract_responsibility_inbox(tenant_id,event_id,payload_hash,received_at) VALUES(?,?,?,UTC_TIMESTAMP(3))`, event.TenantID, event.EventID, hash[:]); err != nil {
		return false, err
	}
	applied := event.SourceVersion > version
	if applied {
		_, err = tx.ExecContext(ctx, `UPDATE settlement_contract_responsibility SET owner_user_id=?,version=?,active=?,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND source_contract_id=?`, event.OwnerUserID, event.SourceVersion, event.Active, event.TenantID, event.SourceContractID)
		if err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return applied, nil
}
