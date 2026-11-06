package reportexport

import "context"

// purgeExpiredContent 用途：清除已过保留期的导出文件字节，避免数据库长期占用大字段。
// 参数：ctx 为工作器轮次上下文。
// 返回值：任一目标表清理失败时返回错误。
//
// 语义：只清空 file_content，不改状态与 expires_at，任务仍保持 READY，
// 下载继续按过期语义返回 410；清理同时限定在本部署租户，不跨租户改写。
func (w *Worker) purgeExpiredContent(ctx context.Context) error {
	if err := w.checkTenant(w.TenantID); err != nil {
		return err
	}
	if _, err := w.DB.ExecContext(ctx, `UPDATE settlement_report_export_job SET file_content=NULL WHERE tenant_id=? AND file_content IS NOT NULL AND expires_at IS NOT NULL AND expires_at<=UTC_TIMESTAMP(3)`, w.TenantID); err != nil {
		return err
	}
	// 效果行只有 PUBLISHED 已发布终态可能遗留字节；PREPARED/INTENT/COMPENSATED
	// 可能仍需原始内容完成补偿或对账，不能在此清除。
	_, err := w.DB.ExecContext(ctx, `UPDATE settlement_report_export_effect e JOIN settlement_report_export_job j ON j.id=e.job_id AND j.tenant_id=e.tenant_id SET e.file_content=NULL WHERE e.tenant_id=? AND e.state='PUBLISHED' AND e.file_content IS NOT NULL AND j.expires_at IS NOT NULL AND j.expires_at<=UTC_TIMESTAMP(3)`, w.TenantID)
	return err
}
