package reportexport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"github.com/j-s-te/settlement/internal/service"
	"io"
	"strings"
	"time"
)

const (
	InvoicedReceivablesCSV = "INVOICED_RECEIVABLES_CSV"
	AgingReceivablesCSV    = "AGING_RECEIVABLES_CSV"
)

type Worker struct {
	DB                   *sql.DB
	WorkerID             string
	TenantID             string
	BatchSize            int
	Gateway              FileGateway
	GatewayMode          string
	GatewayApplicationID string
	LeaseDuration        time.Duration
	HeartbeatInterval    time.Duration
}

// FileGateway 是报表文件网关的最小适配边界，避免 Worker 依赖具体 HTTP 客户端。
type FileGateway interface {
	Upload(context.Context, string, string, string, string, string, io.Reader) (string, error)
	Bind(context.Context, string, string, string, string, string, string) error
	Release(context.Context, string, string, string, string, string) error
	ResolveUpload(context.Context, string) (string, string, bool, error)
	RetireUpload(context.Context, string) (string, string, error)
}

// NormalizeGatewayMode 规范化文件网关模式；空值和未知值均回退到 legacy。
func NormalizeGatewayMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "dual", "required":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "legacy"
	}
}

type job struct {
	id, tenantID, reportType string
	generation               uint64
}

// RunOnce 在生成文件前为待处理任务取得租约，防止多个工作器
// 实例并发发布同一导出。
func (w *Worker) RunOnce(ctx context.Context) error {
	if err := w.checkTenant(w.TenantID); err != nil {
		return err
	}
	limit := w.BatchSize
	if limit < 1 || limit > 20 {
		limit = 5
	}
	var failures []error
	if err := w.reconcileEffects(ctx, limit); err != nil {
		failures = append(failures, err)
	}
	// 执行前立即认领：后续任务不能在等待同批次较早 CSV
	// 生成或网关调用时消耗租约。
	for range limit {
		item, err := w.claim(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			failures = append(failures, err)
			break
		}
		if err := w.generate(ctx, item); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// csv 从单个数据库快照生成全部已放行账务行，避免历史暂存及万行截断。
// 参数：ctx为导出上下文，tenantID为绑定租户。
// 返回值：完整CSV；查询或编码失败返回错误。
func (w *Worker) csv(ctx context.Context, tenantID string) ([]byte, error) {
	rows, err := w.DB.QueryContext(ctx, `SELECT r.receivable_no,cs.source_contract_no,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.original_amount,r.open_amount,r.currency,r.collection_status,r.invoice_status FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id WHERE cs.migration_state='RELEASED' AND r.tenant_id=? AND r.recognition_status='CONFIRMED' AND r.open_amount>0 AND r.invoice_status<>'NOT_INVOICED' ORDER BY r.open_amount DESC,r.due_date ASC,r.id ASC`, tenantID)
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
		values[7] = collectionStatusLabel(values[7])
		values[8] = invoiceStatusLabel(values[8])
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

// agingCSV 用途：按实际租户账龄规则生成可追溯CSV。
// 参数：ctx为任务上下文，tenantID为部署绑定租户。
// 返回值：返回CSV字节；规则、账务读取或编码失败时返回错误。
func (w *Worker) agingCSV(ctx context.Context, tenantID string) ([]byte, error) {
	days, rulesErr := (&service.Service{DB: w.DB}).AgingDays(ctx, tenantID)
	if rulesErr != nil {
		return nil, rulesErr
	}
	bucket, rulesErr := service.BuildAgingCase(days, "r.due_date")
	if rulesErr != nil {
		return nil, rulesErr
	}
	rows, err := w.DB.QueryContext(ctx, `SELECT r.receivable_no,cs.source_contract_no,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.original_amount,r.open_amount,r.currency,GREATEST(TIMESTAMPDIFF(DAY,r.due_date,UTC_DATE()),0),`+bucket+`,r.collection_status,r.invoice_status FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id WHERE cs.migration_state='RELEASED' AND r.tenant_id=? AND r.recognition_status='CONFIRMED' AND r.open_amount>0 ORDER BY r.due_date ASC,r.id ASC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var buffer bytes.Buffer
	buffer.Write([]byte{0xEF, 0xBB, 0xBF})
	writer := csv.NewWriter(&buffer)
	if err := writer.Write([]string{"应收单号", "合同号", "客户", "到期日", "原始应收金额", "未回款金额", "币种", "逾期天数", "账龄区间", "回款状态", "开票状态"}); err != nil {
		return nil, err
	}
	for rows.Next() {
		var values [11]string
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
		values[8] = strings.ReplaceAll(values[8], "-", "–") + " 天"
		if strings.Contains(values[8], "+") {
			values[8] = strings.ReplaceAll(values[8], "+ 天", " 天以上")
		}
		values[9] = collectionStatusLabel(values[9])
		values[10] = invoiceStatusLabel(values[10])
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

func collectionStatusLabel(value string) string {
	return map[string]string{"UNPAID": "未回款", "PARTIALLY_SETTLED": "部分回款", "SETTLED": "已结清"}[value]
}

func invoiceStatusLabel(value string) string {
	return map[string]string{"NOT_INVOICED": "未开票", "PARTIALLY_INVOICED": "部分开票", "FULLY_INVOICED": "已全部开票"}[value]
}

// safeCell 对公式注入载荷加前导单引号（对齐平台 SEC-B6 escapeCSVFormulaCell 口径）：
// 以 =、+、-、@ 开头，或前导含 tab/CR（表格软件会先剥掉这些控制字符再判定公式）
// 的单元格一律加 ' 前缀，使其按字面文本渲染；普通值原样返回，不改变既有导出内容。
func safeCell(value string) string {
	if value == "" {
		return value
	}
	if trimmed := strings.TrimLeft(value, "\t\r"); trimmed != value {
		return "'" + value
	}
	switch value[0] {
	case '=', '+', '-', '@':
		return "'" + value
	default:
		return value
	}
}
