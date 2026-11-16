package service

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"
)

const evidenceOperationTable = "settlement_recognition_evidence"

// checkEvidenceOperation 复核持久证据归属、文件身份与当前操作代次。
// 参数：ctx/tx为短事务，p为操作者，expected为领取前证据快照，token为操作代次。
// 返回值：证据仍属于本次主体与内容时成功，READY重放由调用者提前处理。
func checkEvidenceOperation(ctx context.Context, tx *sql.Tx, p Principal, expected RecognitionEvidence, token string) error {
	current, err := scanRecognitionEvidence(tx.QueryRowContext(ctx, `SELECT `+recognitionEvidenceColumns+` FROM settlement_recognition_evidence WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, expected.ID))
	if err != nil {
		return err
	}
	if current.Status != "PENDING" || current.OrganizationID != expected.OrganizationID || current.CreatedBy != p.UserID || current.CreatedBy != expected.CreatedBy || current.Checksum != expected.Checksum || current.Name != expected.Name || current.MediaType != expected.MediaType || current.ByteSize != expected.ByteSize || (expected.FileID != "" && current.FileID != expected.FileID) || current.ApplicationID != expected.ApplicationID || current.GatewayOrigin != expected.GatewayOrigin {
		return ErrConflict
	}
	if !recognitionAllowed(p, "settlement.recognition.record", current.OrganizationID) {
		return ErrForbidden
	}
	return verifyArchiveOperation(ctx, tx, evidenceOperationTable, p.TenantID, current.ID, token)
}

// uploadRecognitionEvidenceOperation 短事务领取证据，事务外上传绑定，再按代次提交READY。
// 参数：parent/p为请求身份，identity/gateway为原部署边界，in/evidence为已持久化内容。
// 返回值：只有实际业务提交成功才READY，失败保持可同键恢复的PENDING。
func (s *Service) uploadRecognitionEvidenceOperation(parent context.Context, p Principal, identity InvoiceArchiveIdentity, gateway InvoiceArchiveGateway, in RecognitionEvidenceInput, evidence RecognitionEvidence) (out RecognitionEvidence, err error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	current, err := scanRecognitionEvidence(tx.QueryRowContext(ctx, `SELECT `+recognitionEvidenceColumns+` FROM settlement_recognition_evidence WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, evidence.ID))
	if err != nil {
		return out, err
	}
	var recovering bool
	if err = tx.QueryRowContext(ctx, `SELECT recovering FROM settlement_recognition_evidence WHERE tenant_id=? AND id=?`, p.TenantID, evidence.ID).Scan(&recovering); err != nil {
		return out, err
	}
	if recovering {
		if current.ApplicationID != identity.ApplicationID || current.GatewayOrigin != identity.GatewayURL {
			return out, ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return out, err
		}
		return out, errors.Join(ErrConflict, s.recoverRecognitionEvidence(ctx, p, current, gateway))
	}
	if current.ApplicationID != identity.ApplicationID || current.GatewayOrigin != identity.GatewayURL || current.OrganizationID != in.OrganizationID || current.CreatedBy != p.UserID || current.Checksum != evidence.Checksum {
		return out, ErrConflict
	}
	if current.Status == "READY" {
		return current, s.commitInvoiceArchive(ctx, tx)
	}
	if current.Status != "PENDING" {
		return out, ErrConflict
	}
	evidence = current
	token := newID()
	if err = claimArchiveOperation(ctx, tx, evidenceOperationTable, p.TenantID, evidence.ID, token); err != nil {
		return out, err
	}
	if err = s.commitInvoiceArchive(ctx, tx); err != nil {
		return out, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, releaseArchiveOperation(s.DB, evidenceOperationTable, p.TenantID, evidence.ID, token))
		}
	}()
	file, err := gateway.Upload(ctx, "recognition-evidence-"+evidence.ID, identity.ApplicationID, "RECOGNITION_EVIDENCE", evidence.Name, evidence.MediaType, bytes.NewReader(in.Content))
	if err != nil || file == "" {
		return out, errors.Join(ErrDependency, err)
	}
	if err = s.checkpointRecognitionEvidence(ctx, p, in, evidence, token, file); err != nil {
		return out, err
	}
	evidence.FileID = file
	if err = gateway.Bind(ctx, identity.ApplicationID, file, "settlement_recognition_evidence", evidence.ID, "EVIDENCE", evidence.Name); err != nil {
		return out, errors.Join(ErrDependency, err)
	}
	if _, _, err = s.verifyRecognitionDirectory(ctx, p, RecognitionInput{OrganizationID: in.OrganizationID, OrganizationWitnessID: in.OrganizationWitnessID}); err != nil {
		// 暂时无法核验不等于见证失效；保留原意图与稳定网关请求号供同键恢复。
		if errors.Is(err, ErrDependency) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return out, err
		}
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer recoveryCancel()
		evidence.FileID = file
		return out, errors.Join(err, s.retireInvalidRecognitionEvidence(recoveryCtx, p, evidence, token, gateway))
	}
	out, err = s.finishRecognitionEvidence(ctx, p, evidence, token, file)
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrInvoiceArchiveLicense) {
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer recoveryCancel()
		evidence.FileID = file
		err = errors.Join(err, s.retireInvalidRecognitionEvidence(recoveryCtx, p, evidence, token, gateway))
	}
	return out, err
}

// checkpointRecognitionEvidence 上传后重新核对目录归属、许可和文件快照，再允许绑定。
// 参数：ctx/p为请求，in/evidence为原始内容与归属，token/file为当前上传代次与文件。
// 返回值：当前归属及操作代次有效才允许进入绑定阶段。
func (s *Service) checkpointRecognitionEvidence(ctx context.Context, p Principal, in RecognitionEvidenceInput, evidence RecognitionEvidence, token, file string) error {
	if _, _, err := s.verifyRecognitionDirectory(ctx, p, RecognitionInput{OrganizationID: in.OrganizationID, OrganizationWitnessID: in.OrganizationWitnessID}); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkEvidenceOperation(ctx, tx, p, evidence, token); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_recognition_evidence SET file_id=?,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND operation_token=?`, file, p.TenantID, evidence.ID, token); err != nil {
		return err
	}
	return s.commitInvoiceArchive(ctx, tx)
}

// finishRecognitionEvidence 原子提交真实证据READY与审计，不将网关成功当作业务提交。
// 参数：ctx/p为请求，evidence为原领取快照，token/file为当前代次与文件身份。
// 返回值：已提交的READY证据；归属改变、许可失效或审计失败均拒绝提交。
func (s *Service) finishRecognitionEvidence(ctx context.Context, p Principal, evidence RecognitionEvidence, token, file string) (RecognitionEvidence, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return RecognitionEvidence{}, err
	}
	defer tx.Rollback()
	if err = checkEvidenceOperation(ctx, tx, p, evidence, token); err != nil {
		return RecognitionEvidence{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_recognition_evidence SET status='READY',file_id=?,operation_token='',operation_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='PENDING' AND operation_token=?`, file, p.TenantID, evidence.ID, token); err != nil {
		return RecognitionEvidence{}, err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_RECOGNITION_EVIDENCE_READY", "settlement_recognition_evidence", evidence.ID, map[string]any{"actor_id": p.UserID, "organization_id": evidence.OrganizationID, "checksum": evidence.Checksum, "byte_size": evidence.ByteSize}); err != nil {
		return RecognitionEvidence{}, err
	}
	if err = s.commitInvoiceArchive(ctx, tx); err != nil {
		return RecognitionEvidence{}, err
	}
	evidence.Status = "READY"
	evidence.FileID = file
	return evidence, nil
}

// retireInvalidRecognitionEvidence 将失效绑定保留为持久退休意图，并尝试事务外精确补偿。
// 参数：ctx/p为请求，evidence为原上传意图，token为原代次，gateway为原网关。
// 返回值：退休或释放错误；失败保留recovering，后续同键重试继续恢复。
func (s *Service) retireInvalidRecognitionEvidence(ctx context.Context, p Principal, evidence RecognitionEvidence, token string, gateway InvoiceArchiveGateway) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanRecognitionEvidence(tx.QueryRowContext(ctx, `SELECT `+recognitionEvidenceColumns+` FROM settlement_recognition_evidence WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, evidence.ID))
	if err != nil {
		return err
	}
	// 必须先确认未提交；COMMIT响应丢失但已READY的记录禁止补偿。
	if current.Status == "READY" {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE settlement_recognition_evidence SET recovering=TRUE,operation_until=NULL WHERE tenant_id=? AND id=? AND status='PENDING' AND operation_token=?`, p.TenantID, evidence.ID, token)
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
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.recoverRecognitionEvidence(ctx, p, evidence, gateway)
}

// recoverRecognitionEvidence 领取失效证据退休代次，事务外退休和释放后记录终态。
// 参数：ctx/p为当前请求，evidence为持久原网关身份，gateway为同部署网关。
// 返回值：成功补偿无错误，外部失败保留待恢复状态；不伪装为上传成功。
func (s *Service) recoverRecognitionEvidence(ctx context.Context, p Principal, evidence RecognitionEvidence, gateway InvoiceArchiveGateway) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	token := newID()
	result, err := tx.ExecContext(ctx, `UPDATE settlement_recognition_evidence SET operation_token=?,operation_until=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 2 MINUTE) WHERE tenant_id=? AND id=? AND status='PENDING' AND recovering=TRUE AND (operation_until IS NULL OR operation_until<=UTC_TIMESTAMP(3))`, token, p.TenantID, evidence.ID)
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
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.completeRecognitionEvidenceRecovery(ctx, p.TenantID, evidence, token, gateway)
}

// completeRecognitionEvidenceRecovery 在事务外退休并精确释放旧绑定，以操作代次提交清理终态。
// 参数：ctx/tenant为受控上下文，evidence为原意图，token为当前代次，gateway为原网关。
// 返回值：清理或持久化错误；失败保留recovering供后续用户或后台恢复。
func (s *Service) completeRecognitionEvidenceRecovery(ctx context.Context, tenant string, evidence RecognitionEvidence, token string, gateway InvoiceArchiveGateway) error {
	var err error
	if err = s.checkInvoiceArchiveLicense(ctx); err == nil {
		var file, status string
		file, status, err = gateway.RetireUpload(ctx, "recognition-evidence-"+evidence.ID)
		if err == nil && ((file == "" && status != "CANCELLED") || (evidence.FileID != "" && file != evidence.FileID)) {
			err = ErrInvoiceArchiveProvenance
		}
		if err == nil && file != "" {
			if err = s.checkInvoiceArchiveLicense(ctx); err == nil {
				err = gateway.Release(ctx, evidence.ApplicationID, file, "settlement_recognition_evidence", evidence.ID, "EVIDENCE")
			}
		}
	}
	finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err != nil {
		// 推进失败意图的排序时间，避免旧故障长期占满后台每轮20条的领取窗口。
		_, persistErr := s.DB.ExecContext(finish, `UPDATE settlement_recognition_evidence SET operation_token='',operation_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='PENDING' AND recovering=TRUE AND operation_token=?`, tenant, evidence.ID, token)
		return errors.Join(err, persistErr)
	}
	result, err := s.DB.ExecContext(finish, `UPDATE settlement_recognition_evidence SET status='REJECTED',operation_token='',operation_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='PENDING' AND recovering=TRUE AND operation_token=?`, tenant, evidence.ID, token)
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
