package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/j-s-te/settlement/internal/projectreference"
)

// ErrProjectReferenceMismatch 表示权威项目服务在同一版本上把项目映射到了不同合同或客户。
// 已对账的项目引用不允许被静默覆盖，也不允许以通用冲突长期阻塞同步：
// 调用方必须人工核对映射；权威端提升版本后才按新版本受控更新。
var ErrProjectReferenceMismatch = fmt.Errorf("authoritative project reference changed contract at the same version: %w", ErrConflict)

// ErrProjectReferenceInactive 表示权威项目引用当前已失效。
// 本地快照按权威状态收口（active 允许从 true 降为 false），但不得据此新增或刷新协作分配。
var ErrProjectReferenceInactive = fmt.Errorf("authoritative project reference is inactive: %w", ErrInvalid)

// projectReferenceState 是 settlement_project_reference 中需要与权威端比对的字段快照。
type projectReferenceState struct {
	Version    uint64
	ContractID string
	CustomerID string
	Name       string
	Active     bool
}

// decideProjectReferenceUpdate 计算权威引用应如何落地到本地快照。
// 参数：stored 为本地已对账快照，ref 为机器接口复核后的权威引用。
// 返回值：next 为落地后的快照；changed 表示需要更新；error 为不可自动合并的冲突。
// 规则：
//   - 权威版本升高：以新版本为准，允许更换合同/客户，也允许 active 由 true 降为 false；
//   - 同一版本同一合同/客户：只刷新名称与 active，active 可降；
//   - 同一版本更换合同/客户：返回澄清后的冲突，绝不静默覆盖已对账映射；
//   - 权威版本低于本地：拒绝迟到数据，避免缓存回退。
func decideProjectReferenceUpdate(stored projectReferenceState, ref projectreference.Reference) (projectReferenceState, bool, error) {
	next := projectReferenceState{Version: ref.Version, ContractID: ref.ContractID, CustomerID: ref.CustomerID, Name: ref.Name, Active: ref.Active}
	if ref.Version < stored.Version {
		return stored, false, ErrConflict
	}
	if ref.Version == stored.Version && (stored.ContractID != ref.ContractID || stored.CustomerID != ref.CustomerID) {
		return stored, false, fmt.Errorf("%w: project %s version %d stored contract %s/customer %s, authority %s/%s", ErrProjectReferenceMismatch, ref.ProjectID, ref.Version, stored.ContractID, stored.CustomerID, ref.ContractID, ref.CustomerID)
	}
	if next == stored {
		return stored, false, nil
	}
	return next, true, nil
}

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
	if ref.TenantID != p.TenantID || ref.ProjectID != in.ProjectID || ref.ContractID == "" || ref.CustomerID == "" || ref.Version == 0 {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 先按权威版本收口项目引用快照：active 允许随权威端下降（项目关闭/失效），
	// 同版本更换合同仍失败关闭；快照同步不受本次分配是否可继续影响。
	if err = s.saveProjectReference(ctx, tx, p.TenantID, ref); err != nil {
		return err
	}
	if !ref.Active {
		// 权威项目已失效：本地快照已按权威状态收口，但不得据此新增或刷新协作分配。
		// 先提交快照同步，再把请求作为不可分配处理，避免用陈旧 active=TRUE 继续扩大范围。
		if err = tx.Commit(); err != nil {
			return err
		}
		return ErrProjectReferenceInactive
	}
	var contract string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_contract_snapshot WHERE tenant_id=? AND source_contract_id=? AND customer_id=? ORDER BY source_contract_version DESC LIMIT 1 FOR SHARE`, p.TenantID, ref.ContractID, ref.CustomerID).Scan(&contract); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if err = s.saveProjectAssignment(ctx, tx, p, in); err != nil {
		return err
	}
	return tx.Commit()
}

// saveProjectReference 用途：持久保存权威项目引用，拒绝迟到或矛盾的映射。
// 参数：ctx为上下文，tx为事务，tenant为租户，ref为机器接口验证的项目引用。
// 返回值：成功无错误；旧版本返回冲突，同版本更换合同返回澄清后的映射冲突。
func (s *Service) saveProjectReference(ctx context.Context, tx *sql.Tx, tenant string, ref projectreference.Reference) error {
	return saveProjectReferenceTx(ctx, tx, tenant, ref)
}

// saveProjectReferenceTx 用途：与分配入口共用同一快照收口规则，供周期刷新器调用。
// 参数：ctx为上下文，tx为事务，tenant为租户，ref为机器接口验证的项目引用。
// 返回值：成功无错误；旧版本返回冲突，同版本更换合同返回澄清后的映射冲突。
func saveProjectReferenceTx(ctx context.Context, tx *sql.Tx, tenant string, ref projectreference.Reference) error {
	_, err := tx.ExecContext(ctx, `INSERT IGNORE INTO settlement_project_reference(tenant_id,project_id,source_contract_id,customer_id,project_name,version,active,updated_at) VALUES(?,?,?,?,?,?,?,UTC_TIMESTAMP(3))`, tenant, ref.ProjectID, ref.ContractID, ref.CustomerID, ref.Name, ref.Version, ref.Active)
	if err != nil {
		return err
	}
	var stored projectReferenceState
	if err = tx.QueryRowContext(ctx, `SELECT version,source_contract_id,customer_id,project_name,active FROM settlement_project_reference WHERE tenant_id=? AND project_id=? FOR UPDATE`, tenant, ref.ProjectID).Scan(&stored.Version, &stored.ContractID, &stored.CustomerID, &stored.Name, &stored.Active); err != nil {
		return err
	}
	next, changed, err := decideProjectReferenceUpdate(stored, ref)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_project_reference SET source_contract_id=?,customer_id=?,project_name=?,version=?,active=?,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND project_id=?`, next.ContractID, next.CustomerID, next.Name, next.Version, next.Active, tenant, ref.ProjectID)
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
