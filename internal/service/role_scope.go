package service

import (
	"context"
	"database/sql"
	"encoding/json"
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
