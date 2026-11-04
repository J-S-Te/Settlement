package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/j-s-te/settlement/authz"
)

var catalogRoleGrants = loadCatalogRoleGrants()

// loadCatalogRoleGrants 用途：读取嵌入目录的角色权限以隔离不同角色的数据范围。
// 参数：无。
// 返回值：角色权限索引；目录损坏时返回空索引并失败关闭。
func loadCatalogRoleGrants() map[string]map[string]bool {
	var manifest struct {
		Roles []struct {
			Code        string
			Permissions []string
		}
	}
	result := map[string]map[string]bool{}
	if json.Unmarshal(authz.PermissionManifest, &manifest) != nil {
		return result
	}
	for _, role := range manifest.Roles {
		grants := map[string]bool{}
		for _, permission := range role.Permissions {
			grants[permission] = true
		}
		result[role.Code] = grants
	}
	return result
}

// Unrestricted 用途：判断指定能力是否有可信角色授予租户内完整业务范围。
// 参数：permission 为当前操作能力，不得使用无关报表能力代替。
// 返回值：仅明确的全范围角色返回真；销售、助理及未知角色返回假。
func (p Principal) Unrestricted(permission string) bool {
	if !p.Allows(permission) {
		return false
	}
	if p.Permissions["settlement.admin"] {
		return true
	}
	// 仅兼容内部既有组件测试与未版本化主体；在线会话不可走此兼容分支。
	if len(p.Roles) == 0 && p.CatalogVersion == "" {
		return true
	}
	for _, role := range p.Roles {
		if !catalogRoleGrants[role][permission] || role == "settlement_sales" || role == "settlement_project_assistant" {
			continue
		}
		for _, scope := range p.DataScopes {
			if scope.RoleCode == role && (scope.ScopeType == "APPLICATION" || scope.ScopeType == "ENVIRONMENT" || scope.ScopeType == "TENANT") {
				return true
			}
		}
	}
	return false
}

// RequesterScope 用途：固化报表导出申请人创建任务时的授权范围快照，
// 使异步任务重试时仍可核验创建时的授权；不代替执行时在线权限重查。
// 参数：无；只能由 NewRequesterScope 从可信主体生成，或由数据库 JSON 回构。
// 返回值：仅含判定范围必需的授权字段；不保存身份证明细、显示名、用户名和登录IP。
type RequesterScope struct {
	UserID         string      `json:"user_id"`
	Roles          []string    `json:"roles"`
	Permissions    []string    `json:"permissions"`
	DataScopes     []DataScope `json:"data_scopes"`
	CatalogVersion string      `json:"catalog_version"`
}

// NewRequesterScope 用途：从可信在线主体生成可持久化的导出申请人范围快照。
// 参数：p 为已通过入口鉴权的主体。
// 返回值：角色和权限按字典序稳定排列的授权快照，避免同一主体两次快照产生无意义差异。
func NewRequesterScope(p Principal) RequesterScope {
	scope := RequesterScope{UserID: p.UserID, CatalogVersion: p.CatalogVersion}
	scope.Roles = append(scope.Roles, p.Roles...)
	sort.Strings(scope.Roles)
	for permission, allowed := range p.Permissions {
		if allowed {
			scope.Permissions = append(scope.Permissions, permission)
		}
	}
	sort.Strings(scope.Permissions)
	scope.DataScopes = append(scope.DataScopes, p.DataScopes...)
	return scope
}

// Encode 用途：把范围快照编码为导出任务表 requester_scope 列的值。
// 参数：无。
// 返回值：JSON 字节；编码失败时返回错误，调用方不得创建无快照的任务。
func (s RequesterScope) Encode() ([]byte, error) {
	return json.Marshal(s)
}

// DecodeRequesterScope 用途：解码数据库中保存的导出申请人范围快照。
// 参数：raw 为 requester_scope 列原始值，历史任务可能为 NULL。
// 返回值：快照；空值或非法 JSON 返回错误，调用方必须失败关闭而不是按全租户生成。
func DecodeRequesterScope(raw []byte) (RequesterScope, error) {
	var scope RequesterScope
	if len(bytes.TrimSpace(raw)) == 0 {
		return scope, errors.New("requester scope snapshot is missing")
	}
	if err := json.Unmarshal(raw, &scope); err != nil {
		return RequesterScope{}, err
	}
	return scope, nil
}

// Principal 用途：由范围快照回构仅用于范围判定的主体。
// 参数：无。
// 返回值：不含租户、身份证明细与IP的主体；切片为副本，修改结果不回写快照。
func (s RequesterScope) Principal() Principal {
	permissions := make(map[string]bool, len(s.Permissions))
	for _, permission := range s.Permissions {
		permissions[permission] = true
	}
	return Principal{
		UserID:         s.UserID,
		Roles:          append([]string(nil), s.Roles...),
		Permissions:    permissions,
		DataScopes:     append([]DataScope(nil), s.DataScopes...),
		CatalogVersion: s.CatalogVersion,
	}
}

// ContractPredicate 用途：生成指定能力的合同访问谓词，在排序分页前执行。
// 参数：permission 为当前能力，alias 为代码内固定合同快照表别名。
// 返回值：SQL谓词和绑定参数；权限或责任缺失时返回恒假谓词。
func (p Principal) ContractPredicate(permission, alias string) (string, []any) {
	if !p.Allows(permission) {
		return "1=0", nil
	}
	if p.Unrestricted(permission) {
		return "1=1", nil
	}
	if p.UserID == "" || (alias != "cs" && alias != "contract_snapshot") {
		return "1=0", nil
	}
	clauses := []string{}
	args := []any{}
	for _, role := range p.Roles {
		if !catalogRoleGrants[role][permission] {
			continue
		}
		switch role {
		case "settlement_sales":
			clauses = append(clauses, `EXISTS (SELECT 1 FROM settlement_contract_responsibility cr WHERE cr.tenant_id=`+alias+`.tenant_id AND cr.source_contract_id=`+alias+`.source_contract_id AND cr.active=TRUE AND cr.owner_user_id=?)`)
			args = append(args, p.UserID)
		case "settlement_project_assistant":
			clauses = append(clauses, `EXISTS (SELECT 1 FROM settlement_project_assignment pa JOIN settlement_project_reference pr ON pr.tenant_id=pa.tenant_id AND pr.project_id=pa.project_id AND pr.active=TRUE WHERE pa.tenant_id=`+alias+`.tenant_id AND pr.source_contract_id=`+alias+`.source_contract_id AND pa.user_id=? AND pa.active=TRUE)`)
			args = append(args, p.UserID)
		}
	}
	if len(clauses) == 0 {
		return "1=0", nil
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

// RequireContractAccess 用途：在写入或读取详情前核对合同责任范围。
// 参数：ctx 为上下文，p 为可信主体，permission 为能力，snapshotID 为合同快照。
// 返回值：可访问时无错误；不可见统一返回不存在，数据库故障原样返回。
func (s *Service) RequireContractAccess(ctx context.Context, p Principal, permission, snapshotID string) error {
	if !p.Allows(permission) {
		return ErrForbidden
	}
	if p.Unrestricted(permission) {
		return nil
	}
	clause, args := p.ContractPredicate(permission, "cs")
	params := append([]any{p.TenantID, snapshotID}, args...)
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT cs.id FROM settlement_contract_snapshot cs WHERE cs.tenant_id=? AND cs.id=? AND `+clause, params...).Scan(&id)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	return err
}

// RequireContractAccessTx 用途：在业务事务内以锁定责任的方式校验合同访问范围。
// 参数：ctx 为上下文，tx 为当前事务，p 为主体，permission 为能力，snapshotID 为快照。
// 返回值：可访问时无错误，不可见返回不存在，数据库异常原样返回。
func (s *Service) RequireContractAccessTx(ctx context.Context, tx *sql.Tx, p Principal, permission, snapshotID string) error {
	if !p.Allows(permission) {
		return ErrForbidden
	}
	// settlement.revision.resolve 曾以 receivable.adjust 走同一门禁，改用专用动作
	// 权限后必须继续拒绝导入对账未放行（HOLD）的合同，避免收敛动作权限时丢失该拦截。
	if strings.HasPrefix(permission, "settlement.invoice.") || permission == "settlement.receivable.create" || permission == "settlement.receivable.adjust" || permission == "settlement.revision.resolve" {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT migration_state FROM settlement_contract_snapshot WHERE tenant_id=? AND id=?`, p.TenantID, snapshotID).Scan(&state); err != nil {
			return err
		}
		if state == "HOLD" {
			return ErrImportBlocked
		}
	}
	if err := lockImportWrite(ctx, tx, p, snapshotID); err != nil {
		return err
	}
	if p.Unrestricted(permission) {
		return nil
	}
	allowed, err := lockContractResponsibilities(ctx, tx, p, permission, snapshotID)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotFound
	}
	return nil
}

// lockContractResponsibilities 用途：锁定实际责任行，防止授权检查后撤销穿过事务。
// 参数：ctx为上下文，tx为业务事务，p为主体，permission为能力，snapshot为合同快照。
// 返回值：返回实际数据库错误，缺少某类责任仍由最终范围谓词判定。
func lockContractResponsibilities(ctx context.Context, tx *sql.Tx, p Principal, permission, snapshot string) (bool, error) {
	allowed := false
	for _, role := range p.Roles {
		if !catalogRoleGrants[role][permission] {
			continue
		}
		var query string
		switch role {
		case "settlement_sales":
			query = `SELECT cr.source_contract_id FROM settlement_contract_responsibility cr JOIN settlement_contract_snapshot cs ON cs.tenant_id=cr.tenant_id AND cs.source_contract_id=cr.source_contract_id WHERE cs.tenant_id=? AND cs.id=? AND cr.owner_user_id=? AND cr.active=TRUE FOR SHARE`
		case "settlement_project_assistant":
			query = `SELECT pa.project_id FROM settlement_project_assignment pa JOIN settlement_project_reference pr ON pr.tenant_id=pa.tenant_id AND pr.project_id=pa.project_id JOIN settlement_contract_snapshot cs ON cs.tenant_id=pr.tenant_id AND cs.source_contract_id=pr.source_contract_id WHERE cs.tenant_id=? AND cs.id=? AND pa.user_id=? AND pa.active=TRUE AND pr.active=TRUE ORDER BY pa.project_id FOR SHARE`
		default:
			continue
		}
		args := []any{p.TenantID, snapshot}
		args = append(args, p.UserID)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			allowed = true
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return false, err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
	}
	return allowed, nil
}

// scopedAuthorization 用途：判断主体是否携带可校验的角色与目录版本。
// 参数：p 为可信主体。
// 返回值：携带授权上下文时真；未版本化的内部组件/服务测试主体返回假。
func scopedAuthorization(p Principal) bool {
	return len(p.Roles) > 0 || p.CatalogVersion != ""
}

// requireWriteScopeTx 用途：为按ID直接写入的财务单据在业务事务内强制合同责任范围。
// 参数：ctx为上下文，tx为业务事务，p为可信主体，permission为当前写操作能力，snapshotID为数据库中解析出的合同快照。
// 返回值：可写时无错误；缺权限返回禁止，范围不可见返回不存在，快照缺失按不存在关闭。
func (s *Service) requireWriteScopeTx(ctx context.Context, tx *sql.Tx, p Principal, permission, snapshotID string) error {
	// 快照只能来自数据库中的单据关系；解析不到即失败关闭，绝不信任请求体。
	if strings.TrimSpace(snapshotID) == "" {
		return ErrNotFound
	}
	if !scopedAuthorization(p) {
		// 与 Unrestricted 相同的既有兼容分支：仅未版本化的内部组件/服务测试主体
		// 跳过范围解析；在线会话始终携带 catalog_version，不会进入此分支。
		return nil
	}
	return s.RequireContractAccessTx(ctx, tx, p, permission, snapshotID)
}

// requireInvoiceRequestScopeTx 用途：按开票申请主键解析合同快照并校验写范围。
// 参数：ctx为上下文，tx为业务事务，p为可信主体，permission为写能力，requestID为开票申请。
// 返回值：可写时无错误；申请不存在或不可见返回不存在，其余返回实际错误。
func (s *Service) requireInvoiceRequestScopeTx(ctx context.Context, tx *sql.Tx, p Principal, permission, requestID string) error {
	if !scopedAuthorization(p) {
		return nil
	}
	var snapshot string
	err := tx.QueryRowContext(ctx, `SELECT contract_snapshot_id FROM settlement_invoice_request WHERE id=? AND tenant_id=?`, requestID, p.TenantID).Scan(&snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.requireWriteScopeTx(ctx, tx, p, permission, snapshot)
}

// requireTaxInvoiceScopeTx 用途：经税票→开票申请解析合同快照并校验写范围。
// 参数：ctx为上下文，tx为业务事务，p为可信主体，permission为写能力，invoiceID为税票。
// 返回值：可写时无错误；税票不存在或不可见返回不存在，其余返回实际错误。
func (s *Service) requireTaxInvoiceScopeTx(ctx context.Context, tx *sql.Tx, p Principal, permission, invoiceID string) error {
	if !scopedAuthorization(p) {
		return nil
	}
	var snapshot string
	err := tx.QueryRowContext(ctx, `SELECT ir.contract_snapshot_id FROM settlement_tax_invoice ti JOIN settlement_invoice_request ir ON ir.id=ti.invoice_request_id AND ir.tenant_id=ti.tenant_id WHERE ti.id=? AND ti.tenant_id=?`, invoiceID, p.TenantID).Scan(&snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.requireWriteScopeTx(ctx, tx, p, permission, snapshot)
}

// requireReceiptAllocationScopeTx 用途：经核销→应收解析合同快照并校验写范围。
// 参数：ctx为上下文，tx为业务事务，p为可信主体，permission为写能力，allocationID为核销记录。
// 返回值：可写时无错误；核销不存在或不可见返回不存在，其余返回实际错误。
func (s *Service) requireReceiptAllocationScopeTx(ctx context.Context, tx *sql.Tx, p Principal, permission, allocationID string) error {
	if !scopedAuthorization(p) {
		return nil
	}
	var snapshot string
	err := tx.QueryRowContext(ctx, `SELECT rv.contract_snapshot_id FROM settlement_receipt_allocation a JOIN settlement_receivable rv ON rv.id=a.receivable_id AND rv.tenant_id=a.tenant_id WHERE a.id=? AND a.tenant_id=?`, allocationID, p.TenantID).Scan(&snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.requireWriteScopeTx(ctx, tx, p, permission, snapshot)
}

// RequireRecordAccess 用途：通过固定资源关系核验直接ID请求的合同范围。
// 参数：ctx 为上下文，p 为主体，permission 为能力，kind 为固定资源类型，id 为单据ID。
// 返回值：不可见返回不存在，类型非法返回参数错误，其余返回实际查询结果。
func (s *Service) RequireRecordAccess(ctx context.Context, p Principal, permission, kind, id string) error {
	if !p.Allows(permission) {
		return ErrForbidden
	}
	if p.Unrestricted(permission) {
		return nil
	}
	joins := map[string]string{
		"plan":    `settlement_receivable_plan x JOIN settlement_contract_snapshot cs ON cs.id=x.contract_snapshot_id AND cs.tenant_id=x.tenant_id`,
		"invoice": `settlement_tax_invoice x JOIN settlement_invoice_request ir ON ir.id=x.invoice_request_id AND ir.tenant_id=x.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id`,
		"request": `settlement_invoice_request x JOIN settlement_contract_snapshot cs ON cs.id=x.contract_snapshot_id AND cs.tenant_id=x.tenant_id`,
	}
	join, ok := joins[kind]
	if !ok {
		return ErrInvalid
	}
	clause, args := p.ContractPredicate(permission, "cs")
	params := append([]any{p.TenantID, id}, args...)
	var found string
	err := s.DB.QueryRowContext(ctx, `SELECT cs.id FROM `+join+` WHERE x.tenant_id=? AND x.id=? AND `+clause, params...).Scan(&found)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	return err
}
