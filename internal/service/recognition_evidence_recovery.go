package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type evidenceRequestBinding struct {
	Actor, Org, Witness, Name, Media, Checksum, App, Origin string
	Size                                                    int
}

// recognitionEvidenceRequest 统一首次登记与精确清理重放的请求摘要内容。
// 参数：p为原操作者，identity为原网关身份，in/checksum为完整文件与组织见证。
// 返回值：与已发布幂等协议字段及序列一致的绑定对象。
func recognitionEvidenceRequest(p Principal, identity InvoiceArchiveIdentity, in RecognitionEvidenceInput, checksum string) evidenceRequestBinding {
	return evidenceRequestBinding{p.UserID, in.OrganizationID, in.OrganizationWitnessID, in.Name, in.MediaType, checksum, identity.ApplicationID, identity.GatewayURL, len(in.Content)}
}

// replayEvidenceCleanup 仅为精确同键同请求的已持久退休意图恢复清理，不创建或上传文件。
// 参数：ctx/p/key为当前可信身份与原幂等键，identity/gateway为原部署，in/checksum为原文件摘要。
// 返回值：是否匹配待清理意图及清理结果；成功清理仍返回冲突，不冒充上传成功。
func (s *Service) replayEvidenceCleanup(ctx context.Context, p Principal, key string, identity InvoiceArchiveIdentity, gateway InvoiceArchiveGateway, in RecognitionEvidenceInput, checksum string) (bool, error) {
	var stored []byte
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT request_hash,resource_id FROM settlement_idempotency_record WHERE tenant_id=? AND command_type='RECOGNITION_EVIDENCE_UPLOAD' AND idempotency_key=?`, p.TenantID, key).Scan(&stored, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, err := json.Marshal(recognitionEvidenceRequest(p, identity, in, checksum))
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256(raw)
	if !bytes.Equal(stored, hash[:]) {
		return false, ErrConflict
	}
	evidence, err := scanRecognitionEvidence(s.DB.QueryRowContext(ctx, `SELECT `+recognitionEvidenceColumns+` FROM settlement_recognition_evidence WHERE tenant_id=? AND id=? AND status='PENDING' AND recovering=TRUE`, p.TenantID, id))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if evidence.CreatedBy != p.UserID || evidence.ApplicationID != identity.ApplicationID || evidence.GatewayOrigin != identity.GatewayURL {
		return true, ErrConflict
	}
	return true, errors.Join(ErrConflict, s.recoverRecognitionEvidence(ctx, p, evidence, gateway))
}

type RecognitionEvidenceRecoveryWorker struct {
	DB                   *sql.DB
	Identity             InvoiceArchiveIdentity
	Gateway              InvoiceArchiveGateway
	CheckBusinessLicense func(context.Context) error
}

// RunOnce 有界清理已持久recovering证据，撤权人员或失效目录不会阻断技术补偿。
// 参数：ctx为既有后台恢复循环上下文，部署身份和许可证由受控worker装配。
// 返回值：所有清理错误；不上传、不绑定新文件、不处理普通PENDING或READY。
func (w *RecognitionEvidenceRecoveryWorker) RunOnce(ctx context.Context) error {
	if err := w.Identity.validate(w.Identity.TenantID); err != nil {
		return err
	}
	if w.DB == nil || w.Gateway == nil {
		return ErrDependency
	}
	seen := []string{}
	failures := []error{}
	for len(seen) < 20 {
		if w.CheckBusinessLicense != nil {
			if err := w.CheckBusinessLicense(ctx); err != nil {
				return errors.Join(append(failures, err)...)
			}
		}
		id, err := w.recoverOne(ctx, seen)
		if id == "" {
			return errors.Join(append(failures, err)...)
		}
		seen = append(seen, id)
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// recoverOne 短事务领取原部署身份匹配的证据退休代次，然后事务外清理。
// 参数：parent为循环上下文，seen为本轮已处理意图，避免失败记录在同轮热重试。
// 返回值：已领取证据ID及清理错误；无可领取意图时返回空ID。
func (w *RecognitionEvidenceRecoveryWorker) recoverOne(parent context.Context, seen []string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	tx, err := w.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	query := `SELECT ` + recognitionEvidenceColumns + ` FROM settlement_recognition_evidence WHERE tenant_id=? AND application_id=? AND gateway_origin=? AND status='PENDING' AND recovering=TRUE AND (operation_until IS NULL OR operation_until<=UTC_TIMESTAMP(3))`
	args := []any{w.Identity.TenantID, w.Identity.ApplicationID, w.Identity.GatewayURL}
	if len(seen) > 0 {
		query += ` AND id NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(seen)), ",") + `)`
		for _, id := range seen {
			args = append(args, id)
		}
	}
	query += ` ORDER BY updated_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`
	evidence, err := scanRecognitionEvidence(tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	token := newID()
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_recognition_evidence SET operation_token=?,operation_until=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 2 MINUTE) WHERE tenant_id=? AND id=? AND status='PENDING' AND recovering=TRUE`, token, w.Identity.TenantID, evidence.ID); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return evidence.ID, err
	}
	svc := Service{DB: w.DB, CheckInvoiceArchiveLicense: w.CheckBusinessLicense}
	return evidence.ID, svc.completeRecognitionEvidenceRecovery(ctx, w.Identity.TenantID, evidence, token, w.Gateway)
}
