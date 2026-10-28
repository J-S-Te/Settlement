package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
)

type SettlementSettings struct {
	FieldPolicies           map[string]bool `json:"field_policies"`
	ERPStatus               string          `json:"erp_status"`
	AgingDays               []int           `json:"aging_days"`
	TaxRates                []string        `json:"tax_rates"`
	ReminderLeadDays        int             `json:"reminder_lead_days"`
	RepeatDays              int             `json:"repeat_days"`
	EscalationDays          int             `json:"escalation_days"`
	SpecialInvoiceThreshold string          `json:"special_invoice_threshold"`
	Version                 int             `json:"version"`
}

// defaultSettlementSettings 返回业务默认规则。
// 参数：无。
// 返回值：默认设置，金额阈值保持未配置。
func defaultSettlementSettings() SettlementSettings {
	return SettlementSettings{FieldPolicies: map[string]bool{"tax_number_visible": true}, AgingDays: []int{30, 60, 90, 180}, TaxRates: []string{"0", "0.01", "0.03", "0.06", "0.09", "0.13"}, ReminderLeadDays: 3, RepeatDays: 7, EscalationDays: 30}
}

// validateSettlementSettings 校验可持久化规则。
// 参数：in 待保存设置。
// 返回值：非法规则返回输入错误。
func validateSettlementSettings(in SettlementSettings) error {
	if in.Version < 0 {
		return ErrInvalid
	}
	if len(in.AgingDays) != 4 || len(in.TaxRates) == 0 || len(in.TaxRates) > 20 || in.ReminderLeadDays < 0 || in.ReminderLeadDays > 365 || in.RepeatDays < 1 || in.RepeatDays > 365 || in.EscalationDays < 1 || in.EscalationDays > 3650 {
		return ErrInvalid
	}
	for field := range in.FieldPolicies {
		if field != "tax_number_visible" {
			return ErrInvalid
		}
	}
	previous := 0
	for _, day := range in.AgingDays {
		if day <= previous || day > 36500 {
			return ErrInvalid
		}
		previous = day
	}
	for _, rate := range in.TaxRates {
		r, ok := new(big.Rat).SetString(rate)
		if !ok || !sixDecimalPattern.MatchString(rate) || r.Sign() < 0 || r.Cmp(big.NewRat(1, 1)) > 0 {
			return ErrInvalid
		}
	}
	if in.SpecialInvoiceThreshold != "" {
		if _, err := amount(in.SpecialInvoiceThreshold); err != nil {
			return err
		}
	}
	return nil
}

// GetSettlementSettings 读取配置管理员设置。
// 参数：ctx 请求上下文，p 可信主体。
// 返回值：设置或授权及存储错误。
func (s *Service) GetSettlementSettings(ctx context.Context, p Principal) (SettlementSettings, error) {
	if !p.Allows("settlement.settings.manage") {
		return SettlementSettings{}, ErrForbidden
	}
	in, err := s.loadSettlementSettings(ctx, p.TenantID)
	in.ERPStatus = "NOT_CONFIGURED"
	if s.ERPProvider != nil {
		in.ERPStatus = "CONFIGURED"
	}
	return in, err
}

// loadSettlementSettings 读取内部规则及默认值。
// 参数：ctx 上下文，tenant 租户。
// 返回值：实际设置或存储错误。
func (s *Service) loadSettlementSettings(ctx context.Context, tenant string) (SettlementSettings, error) {
	in := defaultSettlementSettings()
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `SELECT settings_json,version FROM settlement_settings WHERE tenant_id=?`, tenant).Scan(&raw, &in.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return in, nil
	}
	if err != nil {
		return in, err
	}
	version := in.Version
	err = json.Unmarshal(raw, &in)
	in.Version = version
	if err == nil {
		err = validateSettlementSettings(in)
	}
	return in, err
}

// PutSettlementSettings 乐观锁保存租户规则并写入审计。
// 参数：ctx 上下文，p 可信管理员，in 新规则及预期版本。
// 返回值：保存后的规则或版本冲突。
func (s *Service) PutSettlementSettings(ctx context.Context, p Principal, in SettlementSettings) (SettlementSettings, error) {
	if !p.Allows("settlement.settings.manage") {
		return in, ErrForbidden
	}
	if err := validateSettlementSettings(in); err != nil {
		return in, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return in, err
	}
	defer tx.Rollback()
	in.ERPStatus = ""
	raw, _ := json.Marshal(in)
	if in.Version == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO settlement_settings(tenant_id,settings_json,version) VALUES(?,?,1)`, p.TenantID, raw)
		if err != nil && mysqlDuplicate(err) {
			return in, ErrConflict
		}
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE settlement_settings SET settings_json=?,version=version+1 WHERE tenant_id=? AND version=?`, raw, p.TenantID, in.Version)
		if err == nil {
			n, _ := result.RowsAffected()
			if n != 1 {
				return in, ErrConflict
			}
		}
	}
	if err != nil {
		return in, err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_SETTINGS_CHANGED", "settings", p.TenantID, map[string]any{"actor_id": p.UserID, "version": in.Version + 1}); err != nil {
		return in, err
	}
	in.Version++
	in.ERPStatus = "NOT_CONFIGURED"
	if s.ERPProvider != nil {
		in.ERPStatus = "CONFIGURED"
	}
	return in, tx.Commit()
}

// specialInvoiceRequired 根据冻结输入判断加签要求。
// 参数：total 单笔金额，threshold 配置阈值，reason 特殊原因。
// 返回值：是否需要总监加签。
func specialInvoiceRequired(total, threshold, reason string) bool {
	return strings.TrimSpace(reason) != "" || (threshold != "" && decimalGreater(total, threshold))
}

// TaxNumberVisible 读取税号明文展示策略。
// 参数：ctx 上下文，tenant 可信租户标识。
// 返回值：税号是否可展示及存储错误。
func (s *Service) TaxNumberVisible(ctx context.Context, tenant string) (bool, error) {
	in, err := s.loadSettlementSettings(ctx, tenant)
	visible, exists := in.FieldPolicies["tax_number_visible"]
	return !exists || visible, err
}

// AgingDays 读取实际账龄分档。
// 参数：ctx 上下文，tenant 可信租户标识。
// 返回值：四个分档边界或存储错误。
func (s *Service) AgingDays(ctx context.Context, tenant string) ([]int, error) {
	in, err := s.loadSettlementSettings(ctx, tenant)
	return in.AgingDays, err
}
