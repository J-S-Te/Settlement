package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ProjectReferenceSyncWorker 用途：按权威项目服务周期重拉项目引用快照，
// 把已换合同/已失效的项目在本地收口为 active/inactive（BIZ-XAPI-05b）。
// 只有 Resolve 成功且事实可合并时才写库；依赖失败不推进刷新水位、也不改 active，
// 下一轮重新拉取。所有写入都是租户内单行幂等更新，重复刷新不会滋生第二份映射。
type ProjectReferenceSyncWorker struct {
	DB        *sql.DB
	Resolver  ProjectReferenceResolver
	BatchSize int
	// CheckBusinessLicense 与其他后台任务一致：拿不到许可即整轮跳过。
	CheckBusinessLicense func(context.Context) error
	// Logf 记录单飞跳过与永久冲突；为空时不额外输出，由调用方日志兜底。
	Logf func(string, ...any)
	// running 是进程内单飞：上一轮未结束时直接跳过，避免同一进程叠加外部调用。
	running sync.Mutex
}

// RunOnce 拉取一批最久未刷新（updated_at 最旧）的项目引用并逐条刷新。
// 参数：ctx 为上下文。
// 返回值：配置错误或首个刷新错误；单飞跳过返回 nil，本轮不产生任何写入。
func (w *ProjectReferenceSyncWorker) RunOnce(ctx context.Context) error {
	if w == nil || w.DB == nil || w.Resolver == nil {
		return errors.New("project reference sync worker requires a database and authority resolver")
	}
	if w.CheckBusinessLicense != nil {
		if err := w.CheckBusinessLicense(ctx); err != nil {
			return err
		}
	}
	limit := w.BatchSize
	if limit < 1 || limit > 200 {
		limit = 50
	}
	if !w.running.TryLock() {
		w.logf("project reference sync skipped: previous run is still in progress")
		return nil
	}
	defer w.running.Unlock()
	rows, err := w.DB.QueryContext(ctx, `SELECT tenant_id,project_id FROM settlement_project_reference ORDER BY updated_at,tenant_id,project_id LIMIT ?`, limit)
	if err != nil {
		return err
	}
	type target struct{ tenant, project string }
	targets := []target{}
	for rows.Next() {
		var item target
		if err = rows.Scan(&item.tenant, &item.project); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var firstErr error
	for _, item := range targets {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = w.refreshProjectReference(ctx, item.tenant, item.project); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// refreshProjectReference 刷新单个项目引用。
// 参数：ctx 为上下文，tenant 为本地可信租户，project 为项目标识。
// 返回值：成功无错误；权威不可达或永久冲突返回错误，且不覆盖已对账事实。
func (w *ProjectReferenceSyncWorker) refreshProjectReference(ctx context.Context, tenant, project string) error {
	tenant = strings.TrimSpace(tenant)
	project = strings.TrimSpace(project)
	if tenant == "" || project == "" {
		return fmt.Errorf("%w: project reference identity is required", ErrInvalid)
	}
	if err := w.checkBusinessLicense(ctx); err != nil {
		return err
	}
	ref, err := w.Resolver.Resolve(ctx, tenant, project)
	if err != nil {
		// 依赖失败不改本地快照：active 与刷新水位都保持原值，下一轮重试。
		return fmt.Errorf("resolve authoritative project reference %s/%s: %w", tenant, project, err)
	}
	if ref.TenantID != tenant || ref.ProjectID != project {
		return fmt.Errorf("%w: project reference identity mismatch for %s/%s", ErrConflict, tenant, project)
	}
	if err := w.checkBusinessLicense(ctx); err != nil {
		return err
	}
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	saveErr := saveProjectReferenceTx(ctx, tx, tenant, ref)
	if saveErr == nil {
		// 即使权威事实无变化也推进刷新水位，让批次在全部项目之间轮转。
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_project_reference SET updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND project_id=?`, tenant, project); err != nil {
			return err
		}
		if err = w.checkBusinessLicense(ctx); err != nil {
			return err
		}
		return tx.Commit()
	}
	if errors.Is(saveErr, ErrProjectReferenceMismatch) || errors.Is(saveErr, ErrConflict) || errors.Is(saveErr, ErrInvalid) {
		// 永久冲突（同版本更换合同/客户、权威版本回退）：保留已对账事实，
		// 只推进刷新水位，避免同一行长期独占批次把其余项目饿死。
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_project_reference SET updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND project_id=?`, tenant, project); err != nil {
			return err
		}
		if err = w.checkBusinessLicense(ctx); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		w.logf("project reference %s/%s kept as stored after conflict: %v", tenant, project, saveErr)
		return saveErr
	}
	return saveErr
}

// checkBusinessLicense 复核项目引用业务同步的当前许可，不缓存上一边界的授权结果。
// 参数：ctx 为后台任务上下文。
// 返回值：未配置回调时无错误；配置后保留真实许可拒绝原因。
func (w *ProjectReferenceSyncWorker) checkBusinessLicense(ctx context.Context) error {
	if w.CheckBusinessLicense != nil {
		return w.CheckBusinessLicense(ctx)
	}
	return nil
}

// logf 输出单飞与冲突日志；未配置日志出口时保持静默。
// 参数：format 为格式串，args 为参数。
// 返回值：无。
func (w *ProjectReferenceSyncWorker) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}
