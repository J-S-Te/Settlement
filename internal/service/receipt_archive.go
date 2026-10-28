package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

type receiptEffect struct{ id, receiptID, documentType, checksum, name, mediaType, app, origin, requestID, fileID, documentID, state string }

// scanReceiptEffect 扫描持久化回单归档意图。
// 参数：row 为数据库查询行。
// 返回值：意图字段及数据库扫描错误。
func scanReceiptEffect(row *sql.Row) (e receiptEffect, err error) {
	err = row.Scan(&e.id, &e.receiptID, &e.documentType, &e.checksum, &e.name, &e.mediaType, &e.app, &e.origin, &e.requestID, &e.fileID, &e.documentID, &e.state)
	return
}

const receiptEffectColumns = `id,receipt_id,document_type,checksum,original_name,media_type,application_id,gateway_origin,request_id,file_id,document_id,state`

// UploadReceiptDocument 通过独立回单用途上传、绑定并原子归档。
// 参数：ctx 上下文，p 财务主体，identity 部署身份，g 网关，in 文件内容及幂等键。
// 返回值：文档标识、是否新建及校验或网关错误。
func (s *Service) UploadReceiptDocument(ctx context.Context, p Principal, identity InvoiceArchiveIdentity, g InvoiceArchiveGateway, in InvoiceArchiveInput) (string, bool, error) {
	if financeAccess(p, "settlement.receipt.record") != nil {
		return "", false, ErrForbidden
	}
	if err := identity.validate(p.TenantID); err != nil {
		return "", false, err
	}
	if g == nil {
		return "", false, fmt.Errorf("%w: file gateway unavailable", ErrInvalid)
	}
	if strings.TrimSpace(in.Key) == "" || len(in.Key) > 512 || in.InvoiceID == "" || in.OriginalName == "" || len(in.OriginalName) > 255 || len(in.MediaType) > 128 || in.DocumentType != "RECEIPT_IMAGE" || len(in.Content) == 0 || len(in.Content) > 10<<20 || in.Checksum != fmt.Sprintf("%x", sha256.Sum256(in.Content)) {
		return "", false, ErrInvalid
	}
	if err := s.checkInvoiceArchiveLicense(ctx); err != nil {
		return "", false, err
	}
	effect, err := s.prepareReceiptArchive(ctx, p, identity, in)
	if err != nil {
		return "", false, err
	}
	// Bound total lock duration; the durable record remains recoverable if the
	// request disconnects, the process dies, or an HTTP response arrives late.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	// Match preparation's invoice -> journal lock order, including the document
	// foreign-key check, to avoid a retry/commit lock inversion.
	var invoice string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_receipt WHERE id=? AND tenant_id=? FOR UPDATE`, in.InvoiceID, p.TenantID).Scan(&invoice); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return "", false, err
	}
	effect, err = scanReceiptEffect(tx.QueryRowContext(ctx, `SELECT `+receiptEffectColumns+` FROM settlement_receipt_archive_effect WHERE id=? AND tenant_id=? FOR UPDATE`, effect.id, p.TenantID))
	if err != nil {
		return "", false, err
	}
	if effect.app != identity.ApplicationID || effect.origin != identity.GatewayURL {
		return "", false, ErrConflict
	}
	if err := s.checkInvoiceArchiveLicense(ctx); err != nil {
		return "", false, err
	}
	if effect.state == "ARCHIVED" {
		return effect.documentID, false, s.commitInvoiceArchive(ctx, tx)
	}
	if effect.state != "PENDING" {
		return "", false, fmt.Errorf("%w: abandoned upload requires a new idempotency key", ErrConflict)
	}
	fileID, err := g.Upload(ctx, effect.requestID, effect.app, "RECEIPT_DOCUMENT", effect.name, effect.mediaType, bytes.NewReader(in.Content))
	if err != nil {
		return "", false, fmt.Errorf("%w: upload: %w", ErrInvoiceArchiveGateway, err)
	}
	if fileID == "" {
		return "", false, errors.New("invoice document upload returned an empty file identity")
	}
	if err := s.checkInvoiceArchiveLicense(ctx); err != nil {
		return "", false, err
	}
	if err = g.Bind(ctx, effect.app, fileID, "receipt", effect.receiptID, "receipt_image", effect.name); err != nil {
		return "", false, fmt.Errorf("%w: bind: %w", ErrInvoiceArchiveGateway, err)
	}
	documentID, err := archiveReceiptDocumentTx(ctx, tx, p, effect.receiptID, effect.documentType, effect.name, effect.mediaType, fileID, effect.checksum)
	if err != nil {
		return "", false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_receipt_archive_effect SET state='ARCHIVED',file_id=?,document_id=?,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND state='PENDING'`, fileID, documentID, effect.id, p.TenantID); err != nil {
		return "", false, err
	}
	return documentID, true, s.commitInvoiceArchive(ctx, tx)
}

// prepareReceiptArchive 在外部上传前持久化可恢复意图。
// 参数：ctx 上下文，p 主体，identity 原始网关身份，in 文件内容。
// 返回值：已有或新建意图及存储错误。
func (s *Service) prepareReceiptArchive(ctx context.Context, p Principal, identity InvoiceArchiveIdentity, in InvoiceArchiveInput) (receiptEffect, error) {
	// Nonexistent independent request keys must not acquire overlapping gap
	// locks. The invoice row serializes content, and the unique key arbitrates
	// same-key requests targeting different invoices.
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return receiptEffect{}, err
	}
	defer tx.Rollback()
	// Locking the invoice serializes checksum reservations without changing the
	// existing document unique constraint or introducing a business state.
	var invoice string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_receipt WHERE id=? AND tenant_id=? FOR UPDATE`, in.InvoiceID, p.TenantID).Scan(&invoice); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return receiptEffect{}, err
	}
	if err := s.checkInvoiceArchiveLicense(ctx); err != nil {
		return receiptEffect{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(in.Key)))
	e, err := scanReceiptEffect(tx.QueryRowContext(ctx, `SELECT `+receiptEffectColumns+` FROM settlement_receipt_archive_effect WHERE tenant_id=? AND key_hash=? FOR UPDATE`, p.TenantID, hash))
	if err == nil {
		if e.receiptID != in.InvoiceID || e.documentType != in.DocumentType || e.checksum != in.Checksum || e.name != in.OriginalName || e.mediaType != in.MediaType || e.app != identity.ApplicationID || e.origin != identity.GatewayURL {
			return receiptEffect{}, ErrConflict
		}
		return e, s.commitInvoiceArchive(ctx, tx)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receiptEffect{}, err
	}
	e = receiptEffect{id: newID(), receiptID: in.InvoiceID, documentType: in.DocumentType, checksum: in.Checksum, name: in.OriginalName, mediaType: in.MediaType, app: identity.ApplicationID, origin: identity.GatewayURL, state: "PENDING"}
	e.requestID = "settlement-receipt-" + e.id
	err = tx.QueryRowContext(ctx, `SELECT id,storage_object_id FROM settlement_receipt_document WHERE tenant_id=? AND receipt_id=? AND document_type=? AND checksum=?`, p.TenantID, in.InvoiceID, in.DocumentType, in.Checksum).Scan(&e.documentID, &e.fileID)
	if err == nil {
		// Existing documents do not encode their gateway origin. A new key must
		// inherit verified provenance, not relabel old bytes as the current app.
		// Pre-journal history stays readable through the original read endpoints;
		// this write path requires controlled reconciliation instead of guessing.
		source, sourceErr := scanReceiptEffect(tx.QueryRowContext(ctx, `SELECT `+receiptEffectColumns+` FROM settlement_receipt_archive_effect WHERE tenant_id=? AND document_id=? AND state='ARCHIVED' AND file_id=? ORDER BY created_at,id LIMIT 1 FOR UPDATE`, p.TenantID, e.documentID, e.fileID))
		if sourceErr != nil && !errors.Is(sourceErr, sql.ErrNoRows) {
			return receiptEffect{}, sourceErr
		}
		if sourceErr != nil || e.fileID == "" || source.fileID != e.fileID || source.documentID != e.documentID || source.receiptID != e.receiptID || source.documentType != e.documentType || source.checksum != e.checksum || source.state != "ARCHIVED" || source.app != identity.ApplicationID || source.origin != identity.GatewayURL {
			return receiptEffect{}, fmt.Errorf("%w: %w", ErrConflict, ErrInvoiceArchiveProvenance)
		}
		e.app, e.origin, e.fileID = source.app, source.origin, source.fileID
		e.state = "ARCHIVED"
	} else if !errors.Is(err, sql.ErrNoRows) {
		return receiptEffect{}, err
	} else {
		var pending string
		err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_receipt_archive_effect WHERE tenant_id=? AND receipt_id=? AND document_type=? AND checksum=? AND state='PENDING' LIMIT 1 FOR UPDATE`, p.TenantID, in.InvoiceID, in.DocumentType, in.Checksum).Scan(&pending)
		if err == nil {
			return receiptEffect{}, fmt.Errorf("%w: identical document is awaiting archive or recovery", ErrConflict)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return receiptEffect{}, err
		}
	}
	if err := s.checkInvoiceArchiveLicense(ctx); err != nil {
		return receiptEffect{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_receipt_archive_effect(id,tenant_id,key_hash,receipt_id,document_type,checksum,original_name,media_type,application_id,gateway_origin,request_id,file_id,document_id,state,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 2 MINUTE),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, e.id, p.TenantID, hash, e.receiptID, e.documentType, e.checksum, e.name, e.mediaType, e.app, e.origin, e.requestID, e.fileID, e.documentID, e.state)
	if err != nil {
		var duplicate *mysql.MySQLError
		if errors.As(err, &duplicate) && duplicate.Number == 1062 {
			return receiptEffect{}, ErrConflict
		}
		return receiptEffect{}, err
	}
	return e, s.commitInvoiceArchive(ctx, tx)
}

type ReceiptArchiveWorker struct {
	DB                   *sql.DB
	Identity             InvoiceArchiveIdentity
	Gateway              InvoiceArchiveGateway
	CheckBusinessLicense func(context.Context) error
}

// RunOnce 有界恢复到期的回单归档意图。
// 参数：ctx 工作循环上下文。
// 返回值：所有恢复错误；无待恢复记录返回空。
func (w *ReceiptArchiveWorker) RunOnce(ctx context.Context) error {
	if err := w.Identity.validate(w.Identity.TenantID); err != nil {
		return err
	}
	if w.Gateway == nil {
		return errors.New("invoice archive recovery gateway unavailable")
	}
	var failures []error
	for range 20 {
		if w.CheckBusinessLicense != nil {
			if err := w.CheckBusinessLicense(ctx); err != nil {
				return errors.Join(append(failures, err)...)
			}
		}
		found, err := w.recoverOne(ctx)
		if err != nil {
			failures = append(failures, err)
		}
		if !found {
			break
		}
	}
	return errors.Join(failures...)
}

// recoverOne 锁定单个意图，撤销上传后精确释放文件绑定。
// 参数：parent 上层任务上下文。
// 返回值：是否发现记录及恢复或持久化错误。
func (w *ReceiptArchiveWorker) recoverOne(parent context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	tx, err := w.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	e, err := scanReceiptEffect(tx.QueryRowContext(ctx, `SELECT `+receiptEffectColumns+` FROM settlement_receipt_archive_effect WHERE tenant_id=? AND state='PENDING' AND available_at<=UTC_TIMESTAMP(3) ORDER BY available_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`, w.Identity.TenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var fileID, status string
	if e.app != w.Identity.ApplicationID || e.origin != w.Identity.GatewayURL {
		err = errors.New("invoice archive recovery configuration differs from durable identity")
	} else {
		if w.CheckBusinessLicense != nil {
			err = w.CheckBusinessLicense(ctx)
		}
		if err == nil {
			fileID, status, err = w.Gateway.RetireUpload(ctx, e.requestID)
		}
		if err == nil && ((fileID == "" && status != "CANCELLED") || (e.fileID != "" && e.fileID != fileID)) {
			err = errors.New("invoice archive retirement identity unresolved")
		}
		if err == nil && fileID != "" {
			if w.CheckBusinessLicense != nil {
				err = w.CheckBusinessLicense(ctx)
			}
			if err == nil {
				err = w.Gateway.Release(ctx, e.app, fileID, "receipt", e.receiptID, "receipt_image")
			}
		}
	}
	if err != nil {
		err = fmt.Errorf("invoice archive recovery %s: %w", e.id, err)
		_, persistErr := tx.ExecContext(ctx, `UPDATE settlement_receipt_archive_effect SET attempts=attempts+1,available_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 30 SECOND),updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=?`, e.id, w.Identity.TenantID)
		if persistErr != nil {
			return false, errors.Join(err, persistErr)
		}
		return true, errors.Join(err, tx.Commit())
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_receipt_archive_effect SET state='RETIRED',file_id=?,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND tenant_id=? AND state='PENDING'`, fileID, e.id, w.Identity.TenantID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// archiveReceiptDocumentTx 原子保存实际回单对象与审计。
// 参数：ctx 上下文，tx 事务，p 主体，receipt 回款，kind 类型，name 原名，media 媒体，file 对象标识，checksum 摘要。
// 返回值：文档标识及持久化错误。
func archiveReceiptDocumentTx(ctx context.Context, tx *sql.Tx, p Principal, receipt, kind, name, media, file, checksum string) (string, error) {
	id := newID()
	_, err := tx.ExecContext(ctx, `INSERT INTO settlement_receipt_document(id,tenant_id,receipt_id,document_type,original_name,media_type,storage_object_id,checksum,created_at) VALUES(?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3))`, id, p.TenantID, receipt, kind, name, media, file, checksum)
	if err != nil {
		return "", err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_RECEIPT_DOCUMENT_ARCHIVED", "receipt", receipt, map[string]any{"actor_id": p.UserID, "document_id": id}); err != nil {
		return "", err
	}
	return id, nil
}
