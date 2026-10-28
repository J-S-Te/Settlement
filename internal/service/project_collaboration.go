package service

import (
	"context"
	"database/sql"
	"strings"

	"github.com/j-s-te/settlement/internal/projectreference"
)

type ProjectReferenceResolver interface {
	Resolve(context.Context, string, string) (projectreference.Reference, error)
}
type RoleDirectory interface {
	ContainsRole(context.Context, string, string) (bool, error)
}
type ProjectAssignmentInput struct {
	ProjectID string `json:"project_id"`
	UserID    string `json:"user_id"`
	Version   uint64 `json:"version"`
	Active    bool   `json:"active"`
}

// AssignProject 用途：由业务管理员分配已验证项目的有效助理，禁止自增范围。
// 参数：ctx为上下文，p为当前主体，in为协作对象与期望版本。
// 返回值：成功时无错误；授权、依赖、实体或版本不满足时返回错误。
func (s *Service) AssignProject(ctx context.Context, p Principal, in ProjectAssignmentInput) error {
	if !p.Allows("settlement.collaboration.manage") || !p.Unrestricted("settlement.collaboration.manage") || (in.Active && p.UserID == in.UserID) {
		return ErrForbidden
	}
	if strings.TrimSpace(in.ProjectID) == "" || strings.TrimSpace(in.UserID) == "" || len(in.ProjectID) > 128 || len(in.UserID) > 128 {
		return ErrInvalid
	}
	if !in.Active {
		return s.revokeProject(ctx, p, in)
	}
	if s.ProjectReferences == nil || s.RoleDirectory == nil {
		return ErrDependency
	}
	valid, err := s.RoleDirectory.ContainsRole(ctx, in.UserID, "settlement_project_assistant")
	if err != nil {
		return ErrDependency
	}
	if !valid {
		return ErrInvalid
	}
	ref, err := s.ProjectReferences.Resolve(ctx, p.TenantID, in.ProjectID)
	if err != nil {
		return ErrDependency
	}
	if ref.TenantID != p.TenantID || ref.ProjectID != in.ProjectID || ref.ContractID == "" || ref.CustomerID == "" || ref.Version == 0 || !ref.Active {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var contract string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_contract_snapshot WHERE tenant_id=? AND source_contract_id=? AND customer_id=? ORDER BY source_contract_version DESC LIMIT 1 FOR SHARE`, p.TenantID, ref.ContractID, ref.CustomerID).Scan(&contract); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if err = s.saveProjectReference(ctx, tx, p.TenantID, ref); err != nil {
		return err
	}
	if err = s.saveProjectAssignment(ctx, tx, p, in); err != nil {
		return err
	}
	return tx.Commit()
}

// saveProjectReference 用途：持久保存权威项目引用，拒绝迟到或矛盾的映射。
// 参数：ctx为上下文，tx为事务，tenant为租户，ref为机器接口验证的项目引用。
// 返回值：成功无错误；旧版本或同版矛盾返回冲突。
func (s *Service) saveProjectReference(ctx context.Context, tx *sql.Tx, tenant string, ref projectreference.Reference) error {
	_, err := tx.ExecContext(ctx, `INSERT IGNORE INTO settlement_project_reference(tenant_id,project_id,source_contract_id,customer_id,project_name,version,active,updated_at) VALUES(?,?,?,?,?,?,TRUE,UTC_TIMESTAMP(3))`, tenant, ref.ProjectID, ref.ContractID, ref.CustomerID, ref.Name, ref.Version)
	if err != nil {
		return err
	}
	var version uint64
	var contract, customer string
	if err = tx.QueryRowContext(ctx, `SELECT version,source_contract_id,customer_id FROM settlement_project_reference WHERE tenant_id=? AND project_id=? FOR UPDATE`, tenant, ref.ProjectID).Scan(&version, &contract, &customer); err != nil {
		return err
	}
	if version > ref.Version || (version == ref.Version && (contract != ref.ContractID || customer != ref.CustomerID)) {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_project_reference SET source_contract_id=?,customer_id=?,project_name=?,version=?,active=TRUE,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND project_id=?`, ref.ContractID, ref.CustomerID, ref.Name, ref.Version, tenant, ref.ProjectID)
	return err
}

// saveProjectAssignment 用途：以乐观版本记录协作分配和审计。
// 参数：ctx为上下文，tx为事务，p为管理主体，in为目标分配。
// 返回值：成功无错误；版本冲突或存储失败返回错误。
func (s *Service) saveProjectAssignment(ctx context.Context, tx *sql.Tx, p Principal, in ProjectAssignmentInput) error {
	var result sql.Result
	var err error
	if in.Version == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO settlement_project_assignment(tenant_id,project_id,user_id,version,active,updated_at,updated_by) VALUES(?,?,?,1,TRUE,UTC_TIMESTAMP(3),?)`, p.TenantID, in.ProjectID, in.UserID, p.UserID)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE settlement_project_assignment SET active=?,version=version+1,updated_at=UTC_TIMESTAMP(3),updated_by=? WHERE tenant_id=? AND project_id=? AND user_id=? AND version=?`, in.Active, p.UserID, p.TenantID, in.ProjectID, in.UserID, in.Version)
	}
	if err != nil {
		if mysqlDuplicate(err) {
			return ErrConflict
		}
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_PROJECT_ASSIGNMENT_CHANGED", "project", in.ProjectID, map[string]any{"actor_id": p.UserID, "user_id": in.UserID, "active": in.Active, "result": "SUCCESS", "risk_level": "HIGH"})
}

// revokeProject 用途：即使目录或项目服务故障仍允许管理者安全撤销既有协作。
// 参数：ctx为上下文，p为管理主体，in为包含现行版本的撤销请求。
// 返回值：成功无错误；缺少版本或并发变更返回错误。
func (s *Service) revokeProject(ctx context.Context, p Principal, in ProjectAssignmentInput) error {
	if in.Version == 0 {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.saveProjectAssignment(ctx, tx, p, in); err != nil {
		return err
	}
	return tx.Commit()
}
