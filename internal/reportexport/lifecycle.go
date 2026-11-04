package reportexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/j-s-te/settlement/internal/service"
)

var errLeaseLost = errors.New("report export lease lost")

// errRequesterScope 标识任务申请人范围快照缺失、被篡改或已不具备全范围导出授权。
// 这类任务由 generate 置 FAILED 后返回该哨兵错误：RunOnce 继续认领同批后续任务，
// 不会因为一条历史坏任务卡住整个租户的导出队列。
var errRequesterScope = errors.New("report export requester scope is not unrestricted")

// requesterScopeFailureMessage 是写入 error_message 的可读原因；
// 不包含快照内容、申请人身份明细或内部错误类型。
const requesterScopeFailureMessage = "导出申请人的数据范围快照缺失或已失效，请重新发起导出"

// reportExportPermission 是申请人快照必须证明的全范围导出能力。
const reportExportPermission = "settlement.report.export"

func (w *Worker) checkTenant(tenantID string) error {
	configured := strings.TrimSpace(w.TenantID)
	if configured == "" {
		return errors.New("report export deployment tenant is required")
	}
	if tenantID != configured {
		return errors.New("report export tenant does not match deployment")
	}
	return nil
}

func (w *Worker) leaseDuration() time.Duration {
	if w.LeaseDuration <= 0 {
		return 2 * time.Minute
	}
	return w.LeaseDuration
}

// claim 以租约栅栏认领当前租户的一项报表导出任务。
// 参数：ctx 为认领上下文。
// 返回值：含申请人快照的任务；无候选返回 sql.ErrNoRows，事务失败返回错误。
func (w *Worker) claim(ctx context.Context) (job, error) {
	if err := w.checkTenant(w.TenantID); err != nil {
		return job{}, err
	}
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return job{}, err
	}
	defer tx.Rollback()
	var item job
	// M-5：连同申请人与其创建任务时的范围快照一起取回；快照不在 SELECT 之外另查，
	// 避免拿到已被其他代次改写的行。校验放在取得租约之后，失败仍走租约栅栏内的 FAILED。
	err = tx.QueryRowContext(ctx, `SELECT id,tenant_id,report_type,lease_generation,requested_by,requester_scope FROM settlement_report_export_job WHERE tenant_id=? AND report_type IN (?,?) AND (status='PENDING' OR (status='PROCESSING' AND locked_until<UTC_TIMESTAMP(3))) ORDER BY created_at ASC LIMIT 1 FOR UPDATE SKIP LOCKED`, w.TenantID, InvoicedReceivablesCSV, AgingReceivablesCSV).Scan(&item.id, &item.tenantID, &item.reportType, &item.generation, &item.requestedBy, &item.requesterScope)
	if err != nil {
		return item, err
	}
	if err = w.checkTenant(item.tenantID); err != nil {
		return item, err
	}
	item.generation++
	result, err := tx.ExecContext(ctx, `UPDATE settlement_report_export_job SET status='PROCESSING',lease_generation=?,locked_by=?,locked_until=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL ? MICROSECOND),started_at=COALESCE(started_at,UTC_TIMESTAMP(3)),updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, item.generation, w.WorkerID, w.leaseDuration().Microseconds(), item.id, item.tenantID)
	if err = changed(result, err); err != nil {
		return item, err
	}
	return item, tx.Commit()
}

func changed(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errLeaseLost
	}
	return nil
}

type exportExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (w *Worker) renew(ctx context.Context, db exportExecer, item job) error {
	if err := w.checkTenant(item.tenantID); err != nil {
		return err
	}
	// MySQL 报告变更行数，因此同一毫秒的两次续租
	// 也必须推进截止时间，否则有效租约会被误判为丢失。
	result, err := db.ExecContext(ctx, `UPDATE settlement_report_export_job SET locked_until=GREATEST(DATE_ADD(UTC_TIMESTAMP(3),INTERVAL ? MICROSECOND),DATE_ADD(locked_until,INTERVAL 1000 MICROSECOND)),updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status='PROCESSING' AND locked_by=? AND lease_generation=? AND locked_until>UTC_TIMESTAMP(3)`, w.leaseDuration().Microseconds(), item.id, item.tenantID, w.WorkerID, item.generation)
	return changed(result, err)
}

// 每次外部调用及发布与接管取得同一个任务行锁。
// 超时工作器在代次被替换后不能再发起调用。
func (w *Worker) leased(ctx context.Context, item job, fn func(*sql.Tx) error) error {
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = w.renew(ctx, tx, item); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (w *Worker) heartbeat(ctx context.Context, item job, cancel context.CancelFunc) func() {
	interval := w.HeartbeatInterval
	if interval <= 0 || interval >= w.leaseDuration() {
		interval = w.leaseDuration() / 4
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.renew(ctx, w.DB, item); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return func() { cancel(); close(stop); <-done }
}

// generate 核验申请人范围快照并在有效租约内生成发布报表。
// 参数：parent 为任务上下文，item 为已认领任务及其租约代次。
// 返回值：生成、网关、授权快照或租约失败时返回错误。
func (w *Worker) generate(parent context.Context, item job) error {
	if err := w.checkTenant(item.tenantID); err != nil {
		return err
	}
	// M-5：生成任何 CSV 之前先验证申请人范围快照。此处已取得租约，失败走租约栅栏内的
	// FAILED，既不生成文件，也不影响同批其他任务继续认领；
	// 在 claim 内校验会把范围语义耦合进纯租约事务，并让租约测试与迁移语义纠缠。
	if err := checkRequesterScope(item); err != nil {
		return w.failWithMessage(parent, item, "requester_scope", requesterScopeFailureMessage, err)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := w.heartbeat(ctx, item, cancel)
	defer stop()
	if err := w.renew(ctx, w.DB, item); err != nil {
		return err
	}
	var content []byte
	var err error
	prefix := "invoiced-receivables"
	if item.reportType == AgingReceivablesCSV {
		content, err = w.agingCSV(ctx, item.tenantID)
		prefix = "aging-receivables"
	} else {
		content, err = w.csv(ctx, item.tenantID)
	}
	if err != nil {
		return w.fail(parent, item, "query", err)
	}
	hash := sha256.Sum256(content)
	filename := fmt.Sprintf("%s-%s-%x.csv", prefix, item.id, hash[:8])
	fileID, fileStatus := "", "DISABLED"
	if w.GatewayMode == "dual" || w.GatewayMode == "required" {
		if w.Gateway == nil {
			if w.GatewayMode == "required" {
				return w.fail(parent, item, "gateway_configuration", errors.New("file gateway unavailable"))
			}
		} else {
			requestID := fmt.Sprintf("settlement-report-%s-%d-%x", item.id, item.generation, hash[:8])
			// PREPARED 证明未发送请求。在 HTTP 前单独提交 INTENT，
			// 防止响应回滚抹去此边界。
			err = w.leased(ctx, item, func(tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, `INSERT INTO settlement_report_export_effect(job_id,lease_generation,tenant_id,application_id,request_id,file_name,file_content,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'PREPARED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, item.id, item.generation, item.tenantID, w.GatewayApplicationID, requestID, filename, content)
				return e
			})
			if err != nil {
				return err
			}
			err = w.leased(ctx, item, func(tx *sql.Tx) error {
				result, e := tx.ExecContext(ctx, `UPDATE settlement_report_export_effect SET state='INTENT',updated_at=UTC_TIMESTAMP(3) WHERE job_id=? AND lease_generation=? AND state='PREPARED'`, item.id, item.generation)
				return changed(result, e)
			})
			if err != nil {
				return err
			}
			err = w.leased(ctx, item, func(tx *sql.Tx) error {
				id, e := w.Gateway.Upload(ctx, requestID, w.GatewayApplicationID, "REPORT_EXPORT", filename, "text/csv; charset=utf-8", bytes.NewReader(content))
				if e != nil {
					return e
				}
				fileID = id
				result, e := tx.ExecContext(ctx, `UPDATE settlement_report_export_effect SET file_id=?,updated_at=UTC_TIMESTAMP(3) WHERE job_id=? AND lease_generation=? AND state='INTENT'`, id, item.id, item.generation)
				return changed(result, e)
			})
			if err == nil {
				err = w.leased(ctx, item, func(_ *sql.Tx) error {
					return w.Gateway.Bind(ctx, w.GatewayApplicationID, fileID, "settlement_report_export", item.id, "REPORT", filename)
				})
			}
			if err != nil {
				if errors.Is(err, errLeaseLost) || ctx.Err() != nil {
					return err
				}
				if w.GatewayMode == "required" {
					return w.fail(parent, item, "gateway", err)
				}
				fileID, fileStatus = "", "FAILED"
			} else {
				fileStatus = "READY"
			}
		}
	}
	err = w.leased(ctx, item, func(tx *sql.Tx) error {
		result, e := tx.ExecContext(ctx, `UPDATE settlement_report_export_job SET status='READY',file_name=?,file_content=?,platform_file_id=?,platform_file_version=CASE WHEN ?<>'' THEN 1 ELSE 0 END,platform_file_sha256=?,platform_file_size=?,platform_file_status=?,expires_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 7 DAY),completed_at=UTC_TIMESTAMP(3),locked_by='',locked_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status='PROCESSING' AND locked_by=? AND lease_generation=? AND locked_until>UTC_TIMESTAMP(3)`, filename, content, fileID, fileID, fmt.Sprintf("%x", hash[:]), len(content), fileStatus, item.id, item.tenantID, w.WorkerID, item.generation)
		if e = changed(result, e); e != nil {
			return e
		}
		if fileStatus == "READY" {
			result, updateErr := tx.ExecContext(ctx, `UPDATE settlement_report_export_effect SET state='PUBLISHED',file_content=NULL,updated_at=UTC_TIMESTAMP(3) WHERE job_id=? AND lease_generation=?`, item.id, item.generation)
			e = changed(result, updateErr)
		}
		return e
	})
	// 失败或结果不明的提交根据已提交任务状态对账；
	// 绝不覆盖新代次，也不猜测文件是否发布成功。
	return err
}

// fail 在有效租约内将报表任务记录为失败。
// 参数：ctx 为上下文，item 为任务，stage 为失败阶段，cause 为内部原因。
// 返回值：状态写入错误或带阶段信息的原始原因。
func (w *Worker) fail(ctx context.Context, item job, stage string, cause error) error {
	return w.failWithMessage(ctx, item, stage, "报表生成失败，请重新发起导出", cause)
}

// failWithMessage 用途：在持有有效租约时把任务置为 FAILED 并写入面向申请人的可读原因。
// 参数：ctx为上下文，item为已认领任务，stage为失败阶段，message为申请人可读原因，cause为内部原因
// 。
// 返回值：任务确实置为 FAILED 时返回带阶段与内部原因的聚合错误；租约已丢失或状态不是本代次
// PROCESSING 时返回实际错误，绝不覆盖新代次。
func (w *Worker) failWithMessage(ctx context.Context, item job, stage, message string, cause error) error {
	slog.ErrorContext(ctx, "report export job failed", "job_id", item.id, "stage", stage, "error_type", fmt.Sprintf("%T", cause))
	result, err := w.DB.ExecContext(ctx, `UPDATE settlement_report_export_job SET status='FAILED',error_message=?,locked_by='',locked_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status='PROCESSING' AND locked_by=? AND lease_generation=? AND locked_until>UTC_TIMESTAMP(3)`, message, item.id, item.tenantID, w.WorkerID, item.generation)
	if err = changed(result, err); err != nil {
		return err
	}
	return fmt.Errorf("report export job %s failed at %s: %w", item.id, stage, cause)
}

// checkRequesterScope 用途：按任务行保存的申请人范围快照判定是否允许全租户导出。
// 参数：item 为已认领任务，含 requested_by 与 requester_scope 原始列值。
// 返回值：快照缺失、JSON 非法、申请人不一致或回构后不再具备全范围导出能力时返回错误，调用方必须失
// 败关闭，绝不按租户全量生成。
func checkRequesterScope(item job) error {
	scope, err := service.DecodeRequesterScope(item.requesterScope)
	if err != nil {
		return fmt.Errorf("%w: %v", errRequesterScope, err)
	}
	if item.requestedBy == "" || scope.UserID != item.requestedBy {
		return fmt.Errorf("%w: requester does not match job owner", errRequesterScope)
	}
	if !scope.Principal().Unrestricted(reportExportPermission) {
		return fmt.Errorf("%w: requester lost unrestricted export scope", errRequesterScope)
	}
	return nil
}
