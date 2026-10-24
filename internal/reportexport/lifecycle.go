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
)

var errLeaseLost = errors.New("report export lease lost")

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
	err = tx.QueryRowContext(ctx, `SELECT id,tenant_id,report_type,lease_generation FROM settlement_report_export_job WHERE tenant_id=? AND report_type IN (?,?) AND (status='PENDING' OR (status='PROCESSING' AND locked_until<UTC_TIMESTAMP(3))) ORDER BY created_at ASC LIMIT 1 FOR UPDATE SKIP LOCKED`, w.TenantID, InvoicedReceivablesCSV, AgingReceivablesCSV).Scan(&item.id, &item.tenantID, &item.reportType, &item.generation)
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

func (w *Worker) generate(parent context.Context, item job) error {
	if err := w.checkTenant(item.tenantID); err != nil {
		return err
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

func (w *Worker) fail(ctx context.Context, item job, stage string, cause error) error {
	slog.ErrorContext(ctx, "report export job failed", "job_id", item.id, "stage", stage, "error_type", fmt.Sprintf("%T", cause))
	result, err := w.DB.ExecContext(ctx, `UPDATE settlement_report_export_job SET status='FAILED',error_message=?,locked_by='',locked_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND status='PROCESSING' AND locked_by=? AND lease_generation=? AND locked_until>UTC_TIMESTAMP(3)`, "报表生成失败，请重新发起导出", item.id, item.tenantID, w.WorkerID, item.generation)
	if err = changed(result, err); err != nil {
		return err
	}
	return fmt.Errorf("report export job %s failed at %s", item.id, stage)
}
