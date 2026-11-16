package service

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type archiveOperation struct {
	table, businessTable, resource, binding, purpose string
	receipt                                          bool
}

var invoiceOperation = archiveOperation{"settlement_invoice_archive_effect", "settlement_tax_invoice", "tax_invoice", "electronic_invoice", "INVOICE_DOCUMENT", false}
var receiptOperation = archiveOperation{"settlement_receipt_archive_effect", "settlement_receipt", "receipt", "receipt_image", "RECEIPT_DOCUMENT", true}

// claimArchiveOperation 领取一个持久操作代次，活跃上传或退休操作不能被重试抢占。
// 参数：ctx/tx为短事务，table为内部固定表，tenant/id为意图身份，token为本轮代次。
// 返回值：领取成功无错误，已有活跃操作或退休意图返回冲突。
func claimArchiveOperation(ctx context.Context, tx *sql.Tx, table, tenant, id, token string) error {
	result, err := tx.ExecContext(ctx, `UPDATE `+table+` SET operation_token=?,operation_until=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 2 MINUTE) WHERE tenant_id=? AND id=? AND recovering=FALSE AND (operation_until IS NULL OR operation_until<=UTC_TIMESTAMP(3))`, token, tenant, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

// verifyArchiveOperation 检查当前持久租约，阻止失去代次的迟到响应提交业务。
// 参数：ctx/tx为短事务，table为内部表，tenant/id/token为领取身份。
// 返回值：当前且未过期的活跃代次成功，其余返回冲突。
func verifyArchiveOperation(ctx context.Context, tx *sql.Tx, table, tenant, id, token string) error {
	var current string
	var active, recovering bool
	err := tx.QueryRowContext(ctx, `SELECT operation_token,COALESCE(operation_until>UTC_TIMESTAMP(3),FALSE),recovering FROM `+table+` WHERE tenant_id=? AND id=? FOR UPDATE`, tenant, id).Scan(&current, &active, &recovering)
	if err != nil {
		return err
	}
	if current != token || !active || recovering {
		return ErrConflict
	}
	return nil
}

// releaseArchiveOperation 在本轮同步外呼结束后释放自己的租约，保留失败意图供重试或恢复。
// 参数：db为连接池，table为内部固定表，tenant/id/token为本轮身份。
// 返回值：释放失败返回错误；不会释放后继操作或推断未知业务提交结果。
func releaseArchiveOperation(db *sql.DB, table, tenant, id, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, `UPDATE `+table+` SET operation_token='',operation_until=NULL WHERE tenant_id=? AND id=? AND operation_token=? AND recovering=FALSE`, tenant, id, token)
	return err
}

// archiveBusinessState 短暂锁定当前业务并检查归档资格，不跨网关调用持锁。
// 参数：ctx/tx为短事务，op为内部归档类型，tenant/id为业务身份。
// 返回值：当前业务状态；不存在或不可归档时拒绝。
func archiveBusinessState(ctx context.Context, tx *sql.Tx, op archiveOperation, tenant, id string) (string, error) {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT status FROM `+op.businessTable+` WHERE id=? AND tenant_id=? FOR UPDATE`, id, tenant).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	valid := state == "ISSUED" || state == "RED_FLUSHED"
	if op.receipt {
		valid = state == "AVAILABLE" || state == "PARTIALLY_ALLOCATED" || state == "FULLY_ALLOCATED"
	}
	if !valid {
		return "", ErrConflict
	}
	return state, nil
}

// loadArchiveOperation 在当前短事务中读取发票或回单意图并规范字段。
// 参数：ctx/tx为短事务，op为类型，tenant/id为精确意图身份。
// 返回值：完整意图及数据库错误。
func loadArchiveOperation(ctx context.Context, tx *sql.Tx, op archiveOperation, tenant, id string) (archiveEffect, error) {
	if !op.receipt {
		return scanArchiveEffect(tx.QueryRowContext(ctx, `SELECT `+archiveEffectColumns+` FROM `+op.table+` WHERE id=? AND tenant_id=? FOR UPDATE`, id, tenant))
	}
	r, err := scanReceiptEffect(tx.QueryRowContext(ctx, `SELECT `+receiptEffectColumns+` FROM `+op.table+` WHERE id=? AND tenant_id=? FOR UPDATE`, id, tenant))
	return archiveEffect{r.id, r.receiptID, r.documentType, r.checksum, r.name, r.mediaType, r.app, r.origin, r.requestID, r.fileID, r.documentID, r.state}, err
}

// checkpointArchiveOperation 复核操作代次、许可及业务状态，在绑定前和业务提交前分别调用。
// 参数：ctx/tx为短事务，op/p/e/token为可信归档身份，state为领取时的业务状态。
// 返回值：仅当前业务状态与代次仍匹配时成功。
func (s *Service) checkpointArchiveOperation(ctx context.Context, tx *sql.Tx, op archiveOperation, p Principal, e archiveEffect, token, state string) error {
	current, err := archiveBusinessState(ctx, tx, op, p.TenantID, e.invoiceID)
	if err != nil {
		return err
	}
	if current != state {
		return ErrConflict
	}
	if err = verifyArchiveOperation(ctx, tx, op.table, p.TenantID, e.id, token); err != nil {
		return err
	}
	row, err := loadArchiveOperation(ctx, tx, op, p.TenantID, e.id)
	if err != nil {
		return err
	}
	if row.state != "PENDING" || row.app != e.app || row.origin != e.origin || row.invoiceID != e.invoiceID {
		return ErrConflict
	}
	return s.checkInvoiceArchiveLicense(ctx)
}

// uploadArchiveOperation 将意图领取、外部I/O及业务提交分为独立阶段。
// 参数：parent为请求，p/identity为可信身份，g为网关，in/e/op为已持久化内容及类型。
// 返回值：只有本地文档与审计提交成功才返回文档成功；失去代次拒绝提交。
func (s *Service) uploadArchiveOperation(parent context.Context, p Principal, identity InvoiceArchiveIdentity, g InvoiceArchiveGateway, in InvoiceArchiveInput, e archiveEffect, op archiveOperation) (document string, created bool, err error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	state, err := archiveBusinessState(ctx, tx, op, p.TenantID, e.invoiceID)
	if err != nil {
		return "", false, err
	}
	e, err = loadArchiveOperation(ctx, tx, op, p.TenantID, e.id)
	if err != nil {
		return "", false, err
	}
	if e.app != identity.ApplicationID || e.origin != identity.GatewayURL {
		return "", false, ErrConflict
	}
	if e.state == "ARCHIVED" {
		return e.documentID, false, s.commitInvoiceArchive(ctx, tx)
	}
	if e.state != "PENDING" {
		return "", false, ErrConflict
	}
	token := newID()
	if err = claimArchiveOperation(ctx, tx, op.table, p.TenantID, e.id, token); err != nil {
		return "", false, err
	}
	if err = s.commitInvoiceArchive(ctx, tx); err != nil {
		return "", false, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, releaseArchiveOperation(s.DB, op.table, p.TenantID, e.id, token))
		}
	}()
	file, err := g.Upload(ctx, e.requestID, e.app, op.purpose, e.name, e.mediaType, bytes.NewReader(in.Content))
	if err != nil {
		return "", false, fmt.Errorf("%w: upload: %w", ErrInvoiceArchiveGateway, err)
	}
	if file == "" {
		return "", false, ErrInvoiceArchiveGateway
	}
	check, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer check.Rollback()
	if err = s.checkpointArchiveOperation(ctx, check, op, p, e, token, state); err != nil {
		return "", false, err
	}
	if _, err = check.ExecContext(ctx, `UPDATE `+op.table+` SET file_id=?,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND operation_token=?`, file, p.TenantID, e.id, token); err != nil {
		return "", false, err
	}
	if err = s.commitInvoiceArchive(ctx, check); err != nil {
		return "", false, err
	}
	if err = g.Bind(ctx, e.app, file, op.resource, e.invoiceID, op.binding, e.name); err != nil {
		return "", false, fmt.Errorf("%w: bind: %w", ErrInvoiceArchiveGateway, err)
	}
	document, created, err = s.finishArchiveOperation(ctx, p, e, op, token, state, file)
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrInvoiceArchiveLicense) {
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelCleanup()
		_, persistErr := s.DB.ExecContext(cleanup, `UPDATE `+op.table+` SET recovering=TRUE,operation_token='',operation_until=NULL,available_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND state='PENDING' AND operation_token=?`, p.TenantID, e.id, token)
		err = errors.Join(err, persistErr)
	}
	return document, created, err
}

// finishArchiveOperation 原子提交业务文档、审计及归档状态，网关成功本身不代表归档完成。
// 参数：ctx/p为请求，e/op为意图，token/state为领取代次和业务状态，file为实际上传对象。
// 返回值：真实文档及提交结果；失败仍保留可恢复意图。
func (s *Service) finishArchiveOperation(ctx context.Context, p Principal, e archiveEffect, op archiveOperation, token, state, file string) (string, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	if err = s.checkpointArchiveOperation(ctx, tx, op, p, e, token, state); err != nil {
		return "", false, err
	}
	var id string
	if op.receipt {
		id, err = archiveReceiptDocumentTx(ctx, tx, p, e.invoiceID, e.documentType, e.name, e.mediaType, file, e.checksum)
	} else {
		id, err = archiveInvoiceDocumentTx(ctx, tx, p, e.invoiceID, e.documentType, e.name, e.mediaType, file, e.checksum)
	}
	if err != nil {
		return "", false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+op.table+` SET state='ARCHIVED',file_id=?,document_id=?,operation_token='',operation_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND state='PENDING' AND operation_token=?`, file, id, e.id, p.TenantID, token)
	if err != nil {
		return "", false, err
	}
	if err = s.commitInvoiceArchive(ctx, tx); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// recoverArchiveOperation 短事务领取退休代次，事务外退休和释放，再短事务记录结果。
// 参数：parent为恢复上下文，db为连接池，identity/g为原网关身份，license为许可检查，op为归档类型。
// 返回值：是否领取意图及恢复错误；未知提交不推断为业务回滚。
func recoverArchiveOperation(parent context.Context, db *sql.DB, identity InvoiceArchiveIdentity, g InvoiceArchiveGateway, license func(context.Context) error, op archiveOperation) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	columns := archiveEffectColumns
	if op.receipt {
		columns = receiptEffectColumns
	}
	query := `SELECT ` + columns + ` FROM ` + op.table + ` WHERE tenant_id=? AND state='PENDING' AND available_at<=UTC_TIMESTAMP(3) AND (operation_until IS NULL OR operation_until<=UTC_TIMESTAMP(3)) ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`
	var e archiveEffect
	if op.receipt {
		r, x := scanReceiptEffect(tx.QueryRowContext(ctx, query, identity.TenantID))
		err = x
		e = archiveEffect{r.id, r.receiptID, r.documentType, r.checksum, r.name, r.mediaType, r.app, r.origin, r.requestID, r.fileID, r.documentID, r.state}
	} else {
		e, err = scanArchiveEffect(tx.QueryRowContext(ctx, query, identity.TenantID))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if e.app != identity.ApplicationID || e.origin != identity.GatewayURL {
		_, err = tx.ExecContext(ctx, `UPDATE `+op.table+` SET attempts=attempts+1,available_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 30 SECOND),updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, e.id, identity.TenantID)
		return true, errors.Join(ErrInvoiceArchiveProvenance, err, tx.Commit())
	}
	token := newID()
	if _, err = tx.ExecContext(ctx, `UPDATE `+op.table+` SET operation_token=?,recovering=TRUE,operation_until=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 2 MINUTE) WHERE id=? AND tenant_id=?`, token, e.id, identity.TenantID); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return true, err
	}
	file, _, operationErr := retireArchiveUpload(ctx, identity, g, license, op, e)
	finish, err := db.BeginTx(ctx, nil)
	if err != nil {
		return true, errors.Join(operationErr, err)
	}
	defer finish.Rollback()
	var result sql.Result
	if operationErr != nil {
		result, err = finish.ExecContext(ctx, `UPDATE `+op.table+` SET attempts=attempts+1,operation_token='',operation_until=NULL,available_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 30 SECOND),updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND state='PENDING' AND recovering=TRUE AND operation_token=?`, e.id, identity.TenantID, token)
	} else {
		result, err = finish.ExecContext(ctx, `UPDATE `+op.table+` SET state='RETIRED',file_id=?,operation_token='',operation_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND state='PENDING' AND recovering=TRUE AND operation_token=?`, file, e.id, identity.TenantID, token)
	}
	if err != nil {
		return true, errors.Join(operationErr, err)
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return true, errors.Join(operationErr, err, ErrConflict)
	}
	return true, errors.Join(operationErr, finish.Commit())
}

// retireArchiveUpload 按稳定请求身份先退休上传，再精确释放原业务绑定。
// 参数：ctx/identity/g为原部署边界，license为许可检查，op/e为持久意图。
// 返回值：退休文件与状态；许可、退休身份或释放失败均保留恢复意图。
func retireArchiveUpload(ctx context.Context, identity InvoiceArchiveIdentity, g InvoiceArchiveGateway, license func(context.Context) error, op archiveOperation, e archiveEffect) (string, string, error) {
	if license != nil {
		if err := license(ctx); err != nil {
			return "", "", err
		}
	}
	file, status, err := g.RetireUpload(ctx, e.requestID)
	if err != nil {
		return "", "", err
	}
	if (file == "" && status != "CANCELLED") || (e.fileID != "" && file != e.fileID) {
		return "", "", ErrInvoiceArchiveProvenance
	}
	if file != "" {
		if license != nil {
			if err = license(ctx); err != nil {
				return file, status, err
			}
		}
		err = g.Release(ctx, identity.ApplicationID, file, op.resource, e.invoiceID, op.binding)
	}
	return file, status, err
}
