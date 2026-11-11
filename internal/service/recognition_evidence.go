package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

type RecognitionEvidenceInput struct {
	OrganizationID, OrganizationWitnessID, Name, MediaType string
	Content                                                []byte
}
type RecognitionEvidence struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
	MediaType      string `json:"media_type"`
	Checksum       string `json:"checksum"`
	ByteSize       int64  `json:"byte_size"`
	Status         string `json:"status"`
	CreatedBy      string `json:"created_by"`
	ApplicationID  string `json:"-"`
	GatewayOrigin  string `json:"-"`
	FileID         string `json:"-"`
}

// validateRecognitionEvidenceInput 检查真实文件魔数、明确类型、大小与安全名称。
// 参数：in为上传文件及所属组织。
// 返回值：格式合法无错误，超限或扩展名与实际内容不符拒绝。
func validateRecognitionEvidenceInput(in RecognitionEvidenceInput) error {
	if in.OrganizationID == "" || in.OrganizationWitnessID == "" || in.Name == "" || len(in.Name) > 255 || filepath.Base(in.Name) != in.Name || strings.ContainsAny(in.Name, "\x00\r\n\\") || len(in.Content) == 0 || len(in.Content) > 10<<20 {
		return ErrInvalid
	}
	actual := http.DetectContentType(in.Content)
	extension := strings.ToLower(filepath.Ext(in.Name))
	allowed := map[string]string{".pdf": "application/pdf", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png"}
	if allowed[extension] == "" || allowed[extension] != in.MediaType || actual != in.MediaType {
		return fmt.Errorf("%w: 财务证据仅支持真实PDF/JPEG/PNG且不超过10MiB", ErrInvalid)
	}
	return nil
}

// scanRecognitionEvidence 扫描同租户独立证据记录，网关内部身份不对浏览器暴露。
// 参数：row为证据数据库行。
// 返回值：真实证据元数据；不存在转换为业务不存在。
func scanRecognitionEvidence(row *sql.Row) (RecognitionEvidence, error) {
	var out RecognitionEvidence
	e := row.Scan(&out.ID, &out.OrganizationID, &out.CreatedBy, &out.Name, &out.MediaType, &out.Checksum, &out.ByteSize, &out.Status, &out.ApplicationID, &out.GatewayOrigin, &out.FileID)
	if errors.Is(e, sql.ErrNoRows) {
		e = ErrNotFound
	}
	return out, e
}

const recognitionEvidenceColumns = `id,organization_id,created_by,name,media_type,checksum,byte_size,status,application_id,gateway_origin,file_id`

// UploadRecognitionEvidence 先持久化上传意图，再按固定请求身份上传绑定，失败可同键安全恢复。
// 参数：ctx/p为当前授权，key为幂等键，identity为现有文件网关部署身份，gateway为真实适配器，in为文件及组织。
// 返回值：只有网关上传绑定及本地提交成功才READY；任何失败保留可重试PENDING。
func (s *Service) UploadRecognitionEvidence(ctx context.Context, p Principal, key string, identity InvoiceArchiveIdentity, gateway InvoiceArchiveGateway, in RecognitionEvidenceInput) (RecognitionEvidence, error) {
	var empty RecognitionEvidence
	if !recognitionAllowed(p, "settlement.recognition.record", in.OrganizationID) {
		return empty, ErrForbidden
	}
	if e := identity.validate(p.TenantID); e != nil {
		return empty, e
	}
	if gateway == nil {
		return empty, ErrDependency
	}
	if e := validateRecognitionEvidenceInput(in); e != nil {
		return empty, e
	}
	if _, _, e := s.verifyRecognitionDirectory(ctx, p, RecognitionInput{OrganizationID: in.OrganizationID, OrganizationWitnessID: in.OrganizationWitnessID}); e != nil {
		return empty, e
	}
	if e := s.checkInvoiceArchiveLicense(ctx); e != nil {
		return empty, e
	}
	key, e := nativeIdempotencyKey(key)
	if e != nil {
		return empty, e
	}
	hash := sha256.Sum256(in.Content)
	checksum := hex.EncodeToString(hash[:])
	evidence, e := s.prepareRecognitionEvidence(ctx, p, key, identity, in, checksum)
	if e != nil {
		return empty, e
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback()
	evidence, e = scanRecognitionEvidence(tx.QueryRowContext(ctx, `SELECT `+recognitionEvidenceColumns+` FROM settlement_recognition_evidence WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, evidence.ID))
	if e != nil {
		return empty, e
	}
	if evidence.ApplicationID != identity.ApplicationID || evidence.GatewayOrigin != identity.GatewayURL || evidence.OrganizationID != in.OrganizationID || evidence.CreatedBy != p.UserID {
		return empty, ErrConflict
	}
	if evidence.Status == "READY" {
		return evidence, s.commitInvoiceArchive(ctx, tx)
	}
	if evidence.Status != "PENDING" {
		return empty, ErrConflict
	}
	file, e := gateway.Upload(ctx, "recognition-evidence-"+evidence.ID, identity.ApplicationID, "RECOGNITION_EVIDENCE", evidence.Name, evidence.MediaType, bytes.NewReader(in.Content))
	if e != nil {
		return empty, ErrDependency
	}
	if file == "" {
		return empty, ErrDependency
	}
	if e = gateway.Bind(ctx, identity.ApplicationID, file, "settlement_recognition_evidence", evidence.ID, "EVIDENCE", evidence.Name); e != nil {
		return empty, ErrDependency
	}
	if _, e = tx.ExecContext(ctx, `UPDATE settlement_recognition_evidence SET status='READY',file_id=?,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='PENDING'`, file, p.TenantID, evidence.ID); e != nil {
		return empty, e
	}
	if e = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_RECOGNITION_EVIDENCE_READY", "settlement_recognition_evidence", evidence.ID, map[string]any{"actor_id": p.UserID, "organization_id": in.OrganizationID, "checksum": checksum, "byte_size": len(in.Content)}); e != nil {
		return empty, e
	}
	if e = s.commitInvoiceArchive(ctx, tx); e != nil {
		return empty, e
	}
	evidence.Status = "READY"
	evidence.FileID = file
	return evidence, nil
}

// prepareRecognitionEvidence 将操作者、组织、文件摘要与部署身份绑定到可恢复上传意图。
// 参数：ctx/p为当前身份，key为幂等键，identity为网关身份，in/checksum为真实内容元数据。
// 返回值：首次意图或精确同内容回放；同键异内容拒绝。
func (s *Service) prepareRecognitionEvidence(ctx context.Context, p Principal, key string, identity InvoiceArchiveIdentity, in RecognitionEvidenceInput, checksum string) (RecognitionEvidence, error) {
	var empty RecognitionEvidence
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback()
	if e = recognitionLock(ctx, tx, p.TenantID); e != nil {
		return empty, e
	}
	request := struct {
		Actor, Org, Witness, Name, Media, Checksum, App, Origin string
		Size                                                    int
	}{p.UserID, in.OrganizationID, in.OrganizationWitnessID, in.Name, in.MediaType, checksum, identity.ApplicationID, identity.GatewayURL, len(in.Content)}
	id, replay, e := claimIdempotency(ctx, tx, p.TenantID, "RECOGNITION_EVIDENCE_UPLOAD", key, newID(), request)
	if e != nil {
		return empty, e
	}
	if !replay {
		_, e = tx.ExecContext(ctx, `INSERT INTO settlement_recognition_evidence(id,tenant_id,organization_id,created_by,name,media_type,checksum,byte_size,status,application_id,gateway_origin,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'PENDING',?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, in.OrganizationID, p.UserID, in.Name, in.MediaType, checksum, len(in.Content), identity.ApplicationID, identity.GatewayURL)
		if e != nil {
			return empty, e
		}
	}
	evidence, e := scanRecognitionEvidence(tx.QueryRowContext(ctx, `SELECT `+recognitionEvidenceColumns+` FROM settlement_recognition_evidence WHERE tenant_id=? AND id=?`, p.TenantID, id))
	if e != nil {
		return empty, e
	}
	if e = tx.Commit(); e != nil {
		return empty, e
	}
	return evidence, nil
}

// GetRecognitionEvidence 逐次验证当前动作与冻结组织，禁止凭任意fileID获取文件。
// 参数：ctx/p为当前身份，id为本地证据引用。
// 返回值：可下载真实证据元数据；未就绪、其它组织或租户统一不存在。
func (s *Service) GetRecognitionEvidence(ctx context.Context, p Principal, id string) (RecognitionEvidence, error) {
	if !recognitionAllowed(p, "settlement.recognition.read", "") {
		return RecognitionEvidence{}, ErrForbidden
	}
	evidence, e := scanRecognitionEvidence(s.DB.QueryRowContext(ctx, `SELECT `+recognitionEvidenceColumns+` FROM settlement_recognition_evidence WHERE tenant_id=? AND id=? AND status='READY'`, p.TenantID, id))
	if e != nil {
		return RecognitionEvidence{}, e
	}
	if !recognitionAllowed(p, "settlement.recognition.read", evidence.OrganizationID) {
		return RecognitionEvidence{}, ErrNotFound
	}
	return evidence, nil
}

// RecordRecognitionEvidenceDownload 在本地审计事务中再次校验组织并记录成功取得文件的访问。
// 参数：ctx/p为当前身份，id为已获取的本地证据引用。
// 返回值：审计提交成功才无错误；无法记录时下载失败关闭。
func (s *Service) RecordRecognitionEvidenceDownload(ctx context.Context, p Principal, id string) error {
	evidence, e := s.GetRecognitionEvidence(ctx, p, id)
	if e != nil {
		return e
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_RECOGNITION_EVIDENCE_DOWNLOADED", "settlement_recognition_evidence", id, map[string]any{"actor_id": p.UserID, "organization_id": evidence.OrganizationID, "checksum": evidence.Checksum}); e != nil {
		return e
	}
	return tx.Commit()
}
