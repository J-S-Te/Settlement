package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"time"
)

type ImportExport struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	SnapshotVersion uint64 `json:"snapshot_version"`
	RowCount        int    `json:"row_count"`
	ReceiptTotal    string `json:"receipt_total"`
	AllocationTotal string `json:"allocation_total"`
	ErrorCode       string `json:"error_code,omitempty"`
	Content         []byte `json:"-"`
}

// importExportRetentionDays 是暂存对账导出可下载内容的保留期；到期后只保留任务摘要。
// 与报表导出的 7 天保留语义一致，避免导出文件字节永久占用数据库。
const importExportRetentionDays = 7

// purgeExpiredImportExports 用途：按租户清理超过保留期的对账导出字节并标记过期。
// 参数：ctx 为上下文，tenant 为可信租户。
// 返回值：清理语句失败时返回错误；只影响本租户已成功且仍持有内容的到期行。
//
// 语义：不新增过期时间列，用 created_at 判定保留期；清空内容并把任务置为 EXPIRED，
// 使下载继续走“非 SUCCEEDED → 409”，绝不返回空成功文件；未到期任务与其它租户不受影响。
func (s *Service) purgeExpiredImportExports(ctx context.Context, tenant string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE settlement_import_export SET content=NULL,status='EXPIRED',error_code='EXPORT_EXPIRED' WHERE tenant_id=? AND status='SUCCEEDED' AND content IS NOT NULL AND created_at<DATE_SUB(UTC_TIMESTAMP(3),INTERVAL ? DAY)`, tenant, importExportRetentionDays)
	return err
}

// CreateImportExport 为授权财务建立可查询的持久导出，失败不会生成空成功文件。
// 参数：ctx为上下文，p为可信财务主体，snapshot为本地导入快照，idempotencyKey为可选重放键。
// 返回值：已生成、持久失败或同键既有任务；授权及创建失败返回错误。
func (s *Service) CreateImportExport(ctx context.Context, p Principal, snapshot, idempotencyKey string) (ImportExport, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	result := ImportExport{ID: newID(), Status: "PENDING"}
	if err := financeAccess(p, "settlement.import.reconcile"); err != nil {
		return result, err
	}
	// 新建前先执行到期清理：同一租户的旧导出不会再被查询时才过期，
	// 清理失败按内部错误失败关闭，不继续生成新文件。
	if err := s.purgeExpiredImportExports(ctx, p.TenantID); err != nil {
		return result, err
	}
	// 重放键与租户、快照、申请人共同派生稳定主键：重复提交命中同一行，
	// 相同键换租户或快照也不会互相复用任务；空键仍按随机主键每次新建。
	if idempotencyKey != "" {
		result.ID = importExportID(p.TenantID, snapshot, p.UserID, idempotencyKey)
	}
	var exists int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_contract_import WHERE tenant_id=? AND contract_snapshot_id=?`, p.TenantID, snapshot).Scan(&exists); err != nil {
		return result, err
	}
	if exists != 1 {
		return result, ErrNotFound
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settlement_import_export(id,tenant_id,contract_snapshot_id,requested_by,status,created_at) VALUES(?,?,?,?,'PENDING',UTC_TIMESTAMP(3))`, result.ID, p.TenantID, snapshot, p.UserID)
	if err != nil {
		if idempotencyKey != "" && mysqlDuplicate(err) {
			// 同键任务已存在：原样返回既有状态（含已失败），不重建、不重试。
			if existing, found, findErr := s.findImportExport(ctx, p.TenantID, snapshot, result.ID); findErr != nil {
				return result, findErr
			} else if found {
				return existing, nil
			}
		}
		return result, err
	}
	err = s.buildImportExport(ctx, p.TenantID, snapshot, &result)
	if err != nil {
		result.Status = "FAILED"
		result.ErrorCode = "RECONCILIATION_EXPORT_FAILED"
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, saveErr := s.DB.ExecContext(recordCtx, `UPDATE settlement_import_export SET status='FAILED',error_code=?,finished_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='PENDING'`, result.ErrorCode, p.TenantID, result.ID)
		if saveErr != nil {
			return result, saveErr
		}
	}
	return result, nil
}

// importExportID 由租户、快照、申请人和重放键派生稳定主键，满足表CHAR(32)且不跨范围复用。
// 参数：tenant为租户，snapshot为导入快照，user为申请人，key为非空重放键。
// 返回值：摘要hex前32位的大写主键。
func importExportID(tenant, snapshot, user, key string) string {
	digest := sha256.Sum256([]byte(tenant + "\x00" + snapshot + "\x00" + user + "\x00" + key))
	return strings.ToUpper(hex.EncodeToString(digest[:]))[:32]
}

// findImportExport 按租户、快照和主键读取既有任务摘要，内容不在POST返回所以不查大字段。
// 参数：ctx为上下文，tenant为租户，snapshot为快照，id为导出任务。
// 返回值：任务摘要、是否存在及存储错误。
func (s *Service) findImportExport(ctx context.Context, tenant, snapshot, id string) (ImportExport, bool, error) {
	var result ImportExport
	err := s.DB.QueryRowContext(ctx, `SELECT id,status,snapshot_version,row_count,receipt_total,allocation_total,error_code FROM settlement_import_export WHERE tenant_id=? AND contract_snapshot_id=? AND id=?`, tenant, snapshot, id).Scan(&result.ID, &result.Status, &result.SnapshotVersion, &result.RowCount, &result.ReceiptTotal, &result.AllocationTotal, &result.ErrorCode)
	if err == sql.ErrNoRows {
		return ImportExport{}, false, nil
	}
	if err != nil {
		return ImportExport{}, false, err
	}
	return result, true, nil
}

// buildImportExport 在共用快照锁下导出全部来源映射，核对总行数和业务金额后发布。
// 参数：ctx为上下文，tenant为租户，snapshot为快照，result为本地导出任务。
// 返回值：快照、凭证、编码或持久化错误，失败事务不发布文件。
func (s *Service) buildImportExport(ctx context.Context, tenant, snapshot string, result *ImportExport) error {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var manifest []byte
	var currency string
	err = tx.QueryRowContext(ctx, `SELECT cs.import_snapshot_version,cs.currency,ci.expected_facts_json FROM settlement_contract_snapshot cs JOIN settlement_contract_import ci ON ci.contract_snapshot_id=cs.id AND ci.tenant_id=cs.tenant_id WHERE cs.tenant_id=? AND cs.id=? FOR SHARE`, tenant, snapshot).Scan(&result.SnapshotVersion, &currency, &manifest)
	if err != nil {
		return err
	}
	var expected int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_import_fact WHERE tenant_id=? AND contract_snapshot_id=?`, tenant, snapshot).Scan(&expected); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.source_system,f.fact_type,f.external_fact_id,f.target_business_id,COALESCE(rc.amount,a.allocated_amount),COALESCE(rc.currency,a.currency) FROM settlement_import_fact f LEFT JOIN settlement_receipt rc ON rc.id=f.target_business_id AND rc.tenant_id=f.tenant_id AND f.fact_type='RECEIPT' LEFT JOIN settlement_receipt_allocation a ON a.id=f.target_business_id AND a.tenant_id=f.tenant_id AND f.fact_type='ALLOCATION' WHERE f.tenant_id=? AND f.contract_snapshot_id=? ORDER BY f.source_system,f.fact_type,f.external_fact_id`, tenant, snapshot)
	if err != nil {
		return err
	}
	var buffer bytes.Buffer
	// 与报表导出 CSV 保持一致：Excel 中文环境需要 UTF-8 BOM 才会按 UTF-8 解析。
	buffer.WriteString("\xef\xbb\xbf")
	writer := csv.NewWriter(&buffer)
	if err = writer.Write([]string{"来源系统", "事实类型", "来源事实ID", "目标业务ID", "金额", "币种"}); err != nil {
		rows.Close()
		return err
	}
	receiptTotal, allocationTotal := new(big.Rat), new(big.Rat)
	for rows.Next() {
		var row [6]string
		targets := []any{&row[0], &row[1], &row[2], &row[3], &row[4], &row[5]}
		if err = rows.Scan(targets...); err != nil {
			rows.Close()
			return err
		}
		if row[5] != currency {
			rows.Close()
			return ErrConflict
		}
		value, e := amount(row[4])
		if e != nil {
			rows.Close()
			return e
		}
		if row[1] == "RECEIPT" {
			receiptTotal.Add(receiptTotal, add(value))
		} else if row[1] == "ALLOCATION" {
			allocationTotal.Add(allocationTotal, add(value))
		} else {
			rows.Close()
			return ErrImportBlocked
		}
		for i := range row {
			row[i] = importCSVCell(row[i])
		}
		if err = writer.Write(row[:]); err != nil {
			rows.Close()
			return err
		}
		result.RowCount++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if result.RowCount != expected {
		return ErrConflict
	}
	var actualReceipt, actualAllocation string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(rc.amount),0) FROM settlement_receipt rc JOIN settlement_import_fact f ON f.target_business_id=rc.id AND f.tenant_id=rc.tenant_id AND f.fact_type='RECEIPT' WHERE f.tenant_id=? AND f.contract_snapshot_id=?`, tenant, snapshot).Scan(&actualReceipt)
	if err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(a.allocated_amount),0) FROM settlement_receipt_allocation a JOIN settlement_import_fact f ON f.target_business_id=a.id AND f.tenant_id=a.tenant_id AND f.fact_type='ALLOCATION' WHERE f.tenant_id=? AND f.contract_snapshot_id=?`, tenant, snapshot).Scan(&actualAllocation)
	if err != nil {
		return err
	}
	if receiptTotal.Cmp(add(actualReceipt)) != 0 || allocationTotal.Cmp(add(actualAllocation)) != 0 {
		return ErrConflict
	}
	result.ReceiptTotal = receiptTotal.FloatString(2)
	result.AllocationTotal = allocationTotal.FloatString(2)
	var facts []ExpectedImportFact
	if err = json.Unmarshal(manifest, &facts); err != nil {
		return err
	}
	if err = writer.Write([]string{"核对版本", strconv.FormatUint(result.SnapshotVersion, 10), "实际行数", strconv.Itoa(result.RowCount), "声明事实数", strconv.Itoa(len(facts))}); err != nil {
		return err
	}
	if err = writer.Write([]string{"到账合计", result.ReceiptTotal, "核销合计", result.AllocationTotal, "币种", currency}); err != nil {
		return err
	}
	writer.Flush()
	if err = writer.Error(); err != nil {
		return err
	}
	published, err := tx.ExecContext(ctx, `UPDATE settlement_import_export SET status='SUCCEEDED',snapshot_version=?,row_count=?,receipt_total=?,allocation_total=?,content=?,finished_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='PENDING'`, result.SnapshotVersion, result.RowCount, result.ReceiptTotal, result.AllocationTotal, buffer.Bytes(), tenant, result.ID)
	if err != nil {
		return err
	}
	if affected, e := published.RowsAffected(); e != nil {
		return e
	} else if affected != 1 {
		return ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	result.Status = "SUCCEEDED"
	return nil
}

// importCSVCell 防止来源字符串在表格软件中被解释为公式。
// 参数：value为未信任的来源文本或精确金额。
// 返回值：保留原文本语义且不会执行公式的CSV单元格。
func importCSVCell(value string) string {
	if value != "" && strings.ContainsAny(value[:1], "=+-@\t\r") {
		return "'" + value
	}
	return value
}

// GetImportExport 查询持久导出状态及已发布内容，保持暂存财务范围隔离。
// 过期的PENDING只在返回值上标记中断，不写库，避免与仍在构建的线程互相覆盖。
// 超过保留期的成功导出在查询前清空内容并标记 EXPIRED，下载因此按冲突拒绝。
// 参数：ctx为上下文，p为可信财务主体，snapshot为快照，id为导出任务。
// 返回值：完整状态及成功文件；越权、缺失或存储故障返回错误。
func (s *Service) GetImportExport(ctx context.Context, p Principal, snapshot, id string) (ImportExport, error) {
	var result ImportExport
	if err := financeAccess(p, "settlement.import.reconcile"); err != nil {
		return result, err
	}
	if err := s.purgeExpiredImportExports(ctx, p.TenantID); err != nil {
		return result, err
	}
	var createdAt time.Time
	err := s.DB.QueryRowContext(ctx, `SELECT id,status,snapshot_version,row_count,receipt_total,allocation_total,error_code,content,created_at FROM settlement_import_export WHERE tenant_id=? AND contract_snapshot_id=? AND id=?`, p.TenantID, snapshot, id).Scan(&result.ID, &result.Status, &result.SnapshotVersion, &result.RowCount, &result.ReceiptTotal, &result.AllocationTotal, &result.ErrorCode, &result.Content, &createdAt)
	if err == sql.ErrNoRows {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	// 仅把超时未完成的PENDING显示为中断：构建线程随后仍可按PENDING提交，
	// 下一次查询即可看到SUCCEEDED，任务内容不会被查询动作改写为失败。
	if result.Status == "PENDING" && createdAt.Before(time.Now().UTC().Add(-5*time.Minute)) {
		result.Status = "FAILED"
		result.ErrorCode = "EXPORT_PROCESS_INTERRUPTED"
	}
	return result, nil
}
