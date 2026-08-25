package reportexport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"time"
)

const InvoicedReceivablesCSV = "INVOICED_RECEIVABLES_CSV"

type Worker struct {
	DB        *sql.DB
	WorkerID  string
	BatchSize int
}

type job struct{ id, tenantID string }

// RunOnce leases pending work before generating files so multiple worker
// instances cannot publish the same export concurrently.
func (w *Worker) RunOnce(ctx context.Context) error {
	limit := w.BatchSize
	if limit < 1 || limit > 20 {
		limit = 5
	}
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,tenant_id FROM settlement_report_export_job WHERE report_type=? AND (status='PENDING' OR (status='PROCESSING' AND locked_until<UTC_TIMESTAMP(3))) ORDER BY created_at ASC LIMIT ? FOR UPDATE SKIP LOCKED`, InvoicedReceivablesCSV, limit)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	jobs := []job{}
	for rows.Next() {
		var item job
		if err := rows.Scan(&item.id, &item.tenantID); err != nil {
			rows.Close()
			_ = tx.Rollback()
			return err
		}
		jobs = append(jobs, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		_ = tx.Rollback()
		return err
	}
	rows.Close()
	for _, item := range jobs {
		if _, err := tx.ExecContext(ctx, `UPDATE settlement_report_export_job SET status='PROCESSING',locked_by=?,locked_until=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 2 MINUTE),started_at=COALESCE(started_at,UTC_TIMESTAMP(3)),updated_at=UTC_TIMESTAMP(3) WHERE id=?`, w.WorkerID, item.id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, item := range jobs {
		w.generate(ctx, item)
	}
	return nil
}

func (w *Worker) generate(ctx context.Context, item job) {
	content, err := w.csv(ctx, item.tenantID)
	if err != nil {
		w.fail(ctx, item.id, err)
		return
	}
	filename := "invoiced-receivables-" + time.Now().UTC().Format("20060102-150405") + ".csv"
	_, err = w.DB.ExecContext(ctx, `UPDATE settlement_report_export_job SET status='READY',file_name=?,file_content=?,expires_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 7 DAY),completed_at=UTC_TIMESTAMP(3),locked_by='',locked_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND status='PROCESSING' AND locked_by=?`, filename, content, item.id, w.WorkerID)
	if err != nil {
		w.fail(ctx, item.id, err)
	}
}

func (w *Worker) csv(ctx context.Context, tenantID string) ([]byte, error) {
	rows, err := w.DB.QueryContext(ctx, `SELECT r.receivable_no,cs.source_contract_no,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.original_amount,r.open_amount,r.currency,r.collection_status,r.invoice_status FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id WHERE r.tenant_id=? AND r.recognition_status='CONFIRMED' AND r.open_amount>0 AND r.invoice_status<>'NOT_INVOICED' ORDER BY r.open_amount DESC,r.due_date ASC,r.id ASC LIMIT 10000`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var buffer bytes.Buffer
	buffer.Write([]byte{0xEF, 0xBB, 0xBF})
	writer := csv.NewWriter(&buffer)
	if err := writer.Write([]string{"应收单号", "合同号", "客户", "到期日", "原始应收金额", "未回款金额", "币种", "回款状态", "开票状态"}); err != nil {
		return nil, err
	}
	for rows.Next() {
		var values [9]string
		pointers := make([]any, len(values))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		for i := range values {
			values[i] = safeCell(values[i])
		}
		if err := writer.Write(values[:]); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func safeCell(value string) string {
	if len(value) > 0 && (value[0] == '=' || value[0] == '+' || value[0] == '-' || value[0] == '@') {
		return "'" + value
	}
	return value
}

func (w *Worker) fail(ctx context.Context, id string, cause error) {
	_, _ = w.DB.ExecContext(ctx, `UPDATE settlement_report_export_job SET status='FAILED',error_message=?,locked_by='',locked_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND locked_by=?`, "报表生成失败，请重新发起导出", id, w.WorkerID)
	_ = cause
}
