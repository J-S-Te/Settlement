package service

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

var ErrCreditSyncNotSubscribed = errors.New("CRM credit sync is not subscribed")

// creditSubscribed 区分正式订阅配置与原有组件的零值构造。
// 参数：无。
// 返回值：nil 保持旧构造兼容，正式配置必须使用明确真假值。
func (s *Service) creditSubscribed() bool {
	return s.CreditSyncSubscribed == nil || *s.CreditSyncSubscribed
}

// creditEligibleSnapshot 只允许订阅中的真实合同适配器产生 CRM 客户事实。
// 参数：ctx/tx 为财务事务，tenant/snapshot 为范围，customer 为已读取客户键。
// 返回值：可投递身份判定；本地客户及来源不明记录不产生任务。
func (s *Service) creditEligibleSnapshot(ctx context.Context, tx *sql.Tx, tenant, snapshot, customer string) (bool, error) {
	if !s.creditSubscribed() {
		return false, nil
	}
	if _, err := strconv.ParseUint(strings.TrimSpace(customer), 10, 64); err != nil {
		return false, nil
	}
	if s.CreditSyncSubscribed == nil {
		return true, nil
	}
	var basis, namespace, source string
	var contractID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT basis_type,customer_namespace,source_application,source_contract_id FROM settlement_contract_snapshot WHERE tenant_id=? AND id=?`, tenant, snapshot).Scan(&basis, &namespace, &source, &contractID)
	if err != nil {
		return false, err
	}
	// 已确认的合同生产者 customer_id 契约来自 CRMCustomerID；legacy 仅按该可信来源兼容，
	// 不将任意数字 ID 或任意来源自动归类为 CRM 客户。
	return basis == "CONTRACT" && source == "contract_management" && contractID.Valid && strings.TrimSpace(contractID.String) != "" &&
		(namespace == "crm" || namespace == "legacy"), nil
}

// validateNativeReceiptCustomer 在本地收款事务中读取服务端客户快照。
// 参数：ctx/tx 为事务，p 为主体，in 为待登记回款。
// 返回值：校验错误；成功时通过in写回服务端名称，非本地引用保留原有行为。
func (s *Service) validateNativeReceiptCustomer(ctx context.Context, tx *sql.Tx, p Principal, in *ReceiptInput) error {
	if !strings.HasPrefix(in.CustomerID, NativeCustomerPrefix) {
		return nil
	}
	if !s.NativeFinancialEnabled {
		return ErrNativeFinancialDisabled
	}
	if !p.Unrestricted("settlement.receipt.record") {
		return ErrForbidden
	}
	if !nativeCustomerPattern.MatchString(in.CustomerID) || in.ImportFact != nil {
		return ErrInvalid
	}
	err := tx.QueryRowContext(ctx, `SELECT name FROM settlement_financial_customer WHERE tenant_id=? AND id=? AND status='ACTIVE' FOR SHARE`, p.TenantID, strings.TrimPrefix(in.CustomerID, NativeCustomerPrefix)).Scan(&in.CustomerName)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
