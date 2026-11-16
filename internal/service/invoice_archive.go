package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-sql-driver/mysql"
)

type InvoiceArchiveGateway interface {
	Upload(context.Context, string, string, string, string, string, io.Reader) (string, error)
	Bind(context.Context, string, string, string, string, string, string) error
	RetireUpload(context.Context, string) (string, string, error)
	Release(context.Context, string, string, string, string, string) error
}

// Identity binds the machine credential deployment to the journal. Configuration
// drift must be reconciled deliberately, never by sending old work to a new app.
type InvoiceArchiveIdentity struct{ TenantID, ApplicationID, GatewayURL string }
type InvoiceArchiveInput struct {
	Key, InvoiceID, DocumentType, OriginalName, MediaType, Checksum string
	Content                                                         []byte
}

var ErrInvoiceArchiveGateway = errors.New("invoice archive gateway operation failed")
var ErrInvoiceArchiveLicense = errors.New("invoice archive commercial license denied")
var ErrInvoiceArchiveProvenance = errors.New("invoice archive original gateway identity cannot be confirmed")

func (s *Service) checkInvoiceArchiveLicense(ctx context.Context) error {
	if s.CheckInvoiceArchiveLicense != nil {
		if err := s.CheckInvoiceArchiveLicense(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrInvoiceArchiveLicense, err)
		}
	}
	return nil
}
func (s *Service) commitInvoiceArchive(ctx context.Context, tx *sql.Tx) error {
	if err := s.checkInvoiceArchiveLicense(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

type archiveEffect struct{ id, invoiceID, documentType, checksum, name, mediaType, app, origin, requestID, fileID, documentID, state string }

func (i InvoiceArchiveIdentity) validate(tenant string) error {
	if i.TenantID == "" || tenant != i.TenantID || i.ApplicationID == "" || i.GatewayURL == "" {
		return fmt.Errorf("%w: invoice archive deployment identity mismatch", ErrForbidden)
	}
	return nil
}

func scanArchiveEffect(row *sql.Row) (e archiveEffect, err error) {
	err = row.Scan(&e.id, &e.invoiceID, &e.documentType, &e.checksum, &e.name, &e.mediaType, &e.app, &e.origin, &e.requestID, &e.fileID, &e.documentID, &e.state)
	return
}

const archiveEffectColumns = `id,tax_invoice_id,document_type,checksum,original_name,media_type,application_id,gateway_origin,request_id,file_id,document_id,state`

// UploadInvoiceDocument 持久化意图后通过独立操作代次上传，网关I/O不占用业务行锁。
// 参数：ctx/p为可信请求，identity/g为原网关部署身份，in为文件与幂等键。
// 返回值：本地文档与审计提交后返回标识和新建标记；失败意图可重试或退休恢复。
func (s *Service) UploadInvoiceDocument(ctx context.Context, p Principal, identity InvoiceArchiveIdentity, g InvoiceArchiveGateway, in InvoiceArchiveInput) (string, bool, error) {
	if !p.Allows("settlement.invoice.issue") {
		return "", false, ErrForbidden
	}
	if err := identity.validate(p.TenantID); err != nil {
		return "", false, err
	}
	if g == nil {
		return "", false, fmt.Errorf("%w: file gateway unavailable", ErrInvalid)
	}
	if strings.TrimSpace(in.Key) == "" || len(in.Key) > 512 || in.InvoiceID == "" || in.OriginalName == "" || len(in.OriginalName) > 255 || len(in.MediaType) > 128 || (in.DocumentType != "ELECTRONIC_INVOICE" && in.DocumentType != "INVOICE_IMAGE") || len(in.Content) == 0 || len(in.Content) > 10<<20 || in.Checksum != fmt.Sprintf("%x", sha256.Sum256(in.Content)) {
		return "", false, ErrInvalid
	}
	if err := s.checkInvoiceArchiveLicense(ctx); err != nil {
		return "", false, err
	}
	effect, err := s.prepareInvoiceArchive(ctx, p, identity, in)
	if err != nil {
		return "", false, err
	}
	return s.uploadArchiveOperation(ctx, p, identity, g, in, effect, invoiceOperation)
}

// prepareInvoiceArchive 在短事务中保留文件摘要及原网关身份，防止重复归档。
// 参数：ctx/p为可信请求，identity为部署身份，in为实际文件与幂等键。
// 返回值：完整持久意图；键或来源冲突时拒绝。
func (s *Service) prepareInvoiceArchive(ctx context.Context, p Principal, identity InvoiceArchiveIdentity, in InvoiceArchiveInput) (archiveEffect, error) {
	// Nonexistent independent request keys must not acquire overlapping gap
	// locks. The invoice row serializes content, and the unique key arbitrates
	// same-key requests targeting different invoices.
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return archiveEffect{}, err
	}
	defer tx.Rollback()
	// Locking the invoice serializes checksum reservations without changing the
	// existing document unique constraint or introducing a business state.
	var invoice string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_tax_invoice WHERE id=? AND tenant_id=? FOR UPDATE`, in.InvoiceID, p.TenantID).Scan(&invoice); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return archiveEffect{}, err
	}
	if err := s.checkInvoiceArchiveLicense(ctx); err != nil {
		return archiveEffect{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(in.Key)))
	e, err := scanArchiveEffect(tx.QueryRowContext(ctx, `SELECT `+archiveEffectColumns+` FROM settlement_invoice_archive_effect WHERE tenant_id=? AND key_hash=? FOR UPDATE`, p.TenantID, hash))
	if err == nil {
		if e.invoiceID != in.InvoiceID || e.documentType != in.DocumentType || e.checksum != in.Checksum || e.name != in.OriginalName || e.mediaType != in.MediaType || e.app != identity.ApplicationID || e.origin != identity.GatewayURL {
			return archiveEffect{}, ErrConflict
		}
		return e, s.commitInvoiceArchive(ctx, tx)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return archiveEffect{}, err
	}
	e = archiveEffect{id: newID(), invoiceID: in.InvoiceID, documentType: in.DocumentType, checksum: in.Checksum, name: in.OriginalName, mediaType: in.MediaType, app: identity.ApplicationID, origin: identity.GatewayURL, state: "PENDING"}
	e.requestID = "settlement-invoice-" + e.id
	err = tx.QueryRowContext(ctx, `SELECT id,storage_object_id FROM settlement_invoice_document WHERE tenant_id=? AND tax_invoice_id=? AND document_type=? AND checksum=?`, p.TenantID, in.InvoiceID, in.DocumentType, in.Checksum).Scan(&e.documentID, &e.fileID)
	if err == nil {
		// Existing documents do not encode their gateway origin. A new key must
		// inherit verified provenance, not relabel old bytes as the current app.
		// Pre-journal history stays readable through the original read endpoints;
		// this write path requires controlled reconciliation instead of guessing.
		source, sourceErr := scanArchiveEffect(tx.QueryRowContext(ctx, `SELECT `+archiveEffectColumns+` FROM settlement_invoice_archive_effect WHERE tenant_id=? AND document_id=? AND state='ARCHIVED' AND file_id=? ORDER BY created_at,id LIMIT 1 FOR UPDATE`, p.TenantID, e.documentID, e.fileID))
		if sourceErr != nil && !errors.Is(sourceErr, sql.ErrNoRows) {
			return archiveEffect{}, sourceErr
		}
		if sourceErr != nil || e.fileID == "" || source.fileID != e.fileID || source.documentID != e.documentID || source.invoiceID != e.invoiceID || source.documentType != e.documentType || source.checksum != e.checksum || source.state != "ARCHIVED" || source.app != identity.ApplicationID || source.origin != identity.GatewayURL {
			return archiveEffect{}, fmt.Errorf("%w: %w", ErrConflict, ErrInvoiceArchiveProvenance)
		}
		e.app, e.origin, e.fileID = source.app, source.origin, source.fileID
		e.state = "ARCHIVED"
	} else if !errors.Is(err, sql.ErrNoRows) {
		return archiveEffect{}, err
	} else {
		var pending string
		err = tx.QueryRowContext(ctx, `SELECT id FROM settlement_invoice_archive_effect WHERE tenant_id=? AND tax_invoice_id=? AND document_type=? AND checksum=? AND state='PENDING' LIMIT 1 FOR UPDATE`, p.TenantID, in.InvoiceID, in.DocumentType, in.Checksum).Scan(&pending)
		if err == nil {
			return archiveEffect{}, fmt.Errorf("%w: identical document is awaiting archive or recovery", ErrConflict)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return archiveEffect{}, err
		}
	}
	if err := s.checkInvoiceArchiveLicense(ctx); err != nil {
		return archiveEffect{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_invoice_archive_effect(id,tenant_id,key_hash,tax_invoice_id,document_type,checksum,original_name,media_type,application_id,gateway_origin,request_id,file_id,document_id,state,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 2 MINUTE),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, e.id, p.TenantID, hash, e.invoiceID, e.documentType, e.checksum, e.name, e.mediaType, e.app, e.origin, e.requestID, e.fileID, e.documentID, e.state)
	if err != nil {
		var duplicate *mysql.MySQLError
		if errors.As(err, &duplicate) && duplicate.Number == 1062 {
			return archiveEffect{}, ErrConflict
		}
		return archiveEffect{}, err
	}
	return e, s.commitInvoiceArchive(ctx, tx)
}

type InvoiceArchiveWorker struct {
	DB                   *sql.DB
	Identity             InvoiceArchiveIdentity
	Gateway              InvoiceArchiveGateway
	CheckBusinessLicense func(context.Context) error
}

func (w *InvoiceArchiveWorker) RunOnce(ctx context.Context) error {
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

// recoverOne 通过持久退休代次恢复意图，外部退休与释放不持数据库行锁。
// 参数：parent为恢复循环上下文。
// 返回值：是否领取意图及退休、释放或本地持久化错误。
func (w *InvoiceArchiveWorker) recoverOne(parent context.Context) (bool, error) {
	return recoverArchiveOperation(parent, w.DB, w.Identity, w.Gateway, w.CheckBusinessLicense, invoiceOperation)
}
