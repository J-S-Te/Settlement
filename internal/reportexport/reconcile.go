package reportexport

import (
	"context"
	"errors"
)

var errUploadUnresolved = errors.New("report export upload outcome is not yet resolved")

// reconcileEffects 重试被遗弃的外部操作。仅释放
// 本日志创建的绑定，绝不释放历史 READY 导出文件。
func (w *Worker) reconcileEffects(ctx context.Context, limit int) error {
	if err := w.checkTenant(w.TenantID); err != nil {
		return err
	}
	rows, err := w.DB.QueryContext(ctx, `SELECT e.job_id,e.lease_generation,e.tenant_id FROM settlement_report_export_effect e JOIN settlement_report_export_job j ON j.id=e.job_id AND j.tenant_id=e.tenant_id WHERE e.tenant_id=? AND e.state IN ('PREPARED','INTENT') AND (e.available_at IS NULL OR e.available_at<=UTC_TIMESTAMP(3)) AND NOT (j.status='PROCESSING' AND j.lease_generation=e.lease_generation AND j.locked_until>UTC_TIMESTAMP(3)) ORDER BY e.updated_at ASC LIMIT ?`, w.TenantID, limit)
	if err != nil {
		return err
	}
	var items []job
	for rows.Next() {
		var item job
		if err = rows.Scan(&item.id, &item.generation, &item.tenantID); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, item := range items {
		if err = w.reconcileEffect(ctx, item); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (w *Worker) reconcileEffect(ctx context.Context, item job) error {
	if err := w.checkTenant(item.tenantID); err != nil {
		return err
	}
	if w.Gateway == nil {
		return errors.New("file gateway unavailable for export reconciliation")
	}
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, publishedID string
	var generation uint64
	var alive bool
	err = tx.QueryRowContext(ctx, `SELECT status,lease_generation,platform_file_id,COALESCE(locked_until>UTC_TIMESTAMP(3),0) FROM settlement_report_export_job WHERE id=? AND tenant_id=? FOR UPDATE`, item.id, item.tenantID).Scan(&status, &generation, &publishedID, &alive)
	if err != nil {
		return err
	}
	if status == "PROCESSING" && generation == item.generation && alive {
		return nil
	}
	var applicationID, requestID, filename, fileID, state string
	var content []byte
	err = tx.QueryRowContext(ctx, `SELECT application_id,request_id,file_name,file_content,file_id,state FROM settlement_report_export_effect WHERE job_id=? AND lease_generation=? AND tenant_id=? FOR UPDATE`, item.id, item.generation, item.tenantID).Scan(&applicationID, &requestID, &filename, &content, &fileID, &state)
	if err != nil {
		return err
	}
	if state != "INTENT" && state != "PREPARED" {
		return nil
	}
	if applicationID != w.GatewayApplicationID {
		return errors.New("report export effect application does not match deployment")
	}
	if status == "READY" && generation == item.generation && publishedID != "" && publishedID == fileID {
		_, err = tx.ExecContext(ctx, `UPDATE settlement_report_export_effect SET state='PUBLISHED',file_content=NULL,updated_at=UTC_TIMESTAMP(3) WHERE job_id=? AND lease_generation=?`, item.id, item.generation)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	// 禁止重新上传已退役内容。失败网关预留对该键而言为终态，
	// 重新传输可能创建新的未使用对象。
	if err == nil {
		// 直接按会话身份 Retire；已清理文件的严格 Resolve 会返回
		// 409，不能把可读性检查作为补偿前置。Retire 取得预留锁并
		// 持久化墓碑，返回稳定文件身份或权威 CANCELLED。
		// 进行中的 WRITING 请求之后可能完成字节写入，但
		// 下方 Release 会隔离精确文件及资源绑定。
		var retiredID, retiredStatus string
		retiredID, retiredStatus, err = w.Gateway.RetireUpload(ctx, requestID)
		if err == nil {
			if (fileID != "" && fileID != retiredID) || (retiredID == "" && retiredStatus != "CANCELLED") {
				err = errUploadUnresolved
			} else {
				fileID = retiredID
			}
		}
	}
	if err == nil && fileID != "" {
		err = w.Gateway.Release(ctx, applicationID, fileID, "settlement_report_export", item.id, "REPORT")
	}
	if err != nil {
		// 即使释放失败，也持久化已解析 ID 及持久重试；
		// 尽力清理不能抹去未经确认的外部结果。
		_, persistErr := tx.ExecContext(ctx, `UPDATE settlement_report_export_effect SET file_id=?,attempts=attempts+1,available_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 30 SECOND),updated_at=UTC_TIMESTAMP(3) WHERE job_id=? AND lease_generation=?`, fileID, item.id, item.generation)
		if persistErr != nil {
			return errors.Join(err, persistErr)
		}
		return errors.Join(err, tx.Commit())
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_report_export_effect SET file_id=?,state='COMPENSATED',file_content=NULL,available_at=NULL,updated_at=UTC_TIMESTAMP(3) WHERE job_id=? AND lease_generation=?`, fileID, item.id, item.generation)
	if err != nil {
		return err
	}
	return tx.Commit()
}
