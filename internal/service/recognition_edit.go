package service

import (
	"context"
	"encoding/json"
	"time"
)

type RecognitionEdit struct {
	RecognitionInput
	ExpectedVersion uint64 `json:"expected_version"`
}

// UpdateRecognition 修改本人草稿或退回记录，保持稳定来源身份并记录新的不可变版本事件。
// 参数：ctx/p为当前授权，id为本人记录，key为幂等键，in为完整内容及预期版本。
// 返回值：更新后草稿；已确认、跨范围、他人记录或来源变更拒绝。
func (s *Service) UpdateRecognition(ctx context.Context, p Principal, id, key string, in RecognitionEdit) (Recognition, error) {
	var empty Recognition
	if !recognitionAllowed(p, "settlement.recognition.record", in.OrganizationID) {
		return empty, ErrForbidden
	}
	if e := validateRecognition(in.RecognitionInput, time.Now()); e != nil {
		return empty, e
	}
	before, e := s.GetRecognition(ctx, p, id, "settlement.recognition.record")
	if e != nil {
		return empty, e
	}
	if before.CreatedBy != p.UserID {
		return empty, ErrForbidden
	}
	if before.Status != "DRAFT" && before.Status != "RETURNED" {
		return empty, ErrConflict
	}
	oldIdentity, _ := json.Marshal([]string{before.Kind, before.SourceSystem, before.SourceEnvironment, before.SourceFactID, before.SourceClaimID, before.SourceEffectID, before.SourceItemID, before.SourceAllocationKey, before.OriginalID})
	newIdentity, _ := json.Marshal([]string{in.Kind, in.SourceSystem, in.SourceEnvironment, in.SourceFactID, in.SourceClaimID, in.SourceEffectID, in.SourceItemID, in.SourceAllocationKey, in.OriginalID})
	if string(oldIdentity) != string(newIdentity) {
		return empty, ErrConflict
	}
	org, name, e := s.verifyRecognitionDirectory(ctx, p, in.RecognitionInput)
	if e != nil {
		return empty, e
	}
	project, e := s.verifyRecognitionProject(ctx, p, in.RecognitionInput)
	if e != nil {
		return empty, e
	}
	var claim CostClaim
	if in.SourceClaimID != "" && in.OriginalID == "" {
		claim, e = s.recognitionClaim(ctx, p, in.RecognitionInput)
		if e != nil {
			return empty, e
		}
	}
	key, e = nativeIdempotencyKey(key)
	if e != nil {
		return empty, e
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback()
	if e = recognitionLock(ctx, tx, p.TenantID); e != nil {
		return empty, e
	}
	row, e := loadRecognition(ctx, tx, p.TenantID, id)
	if e != nil {
		return empty, e
	}
	_, replay, e := claimIdempotency(ctx, tx, p.TenantID, "RECOGNITION_EDIT", key, id, struct {
		Actor, ID string
		Input     RecognitionEdit
	}{p.UserID, id, in})
	if e != nil {
		return empty, e
	}
	if replay {
		return row, nil
	}
	if row.Version != in.ExpectedVersion || row.Status != "DRAFT" && row.Status != "RETURNED" {
		return empty, ErrConflict
	}
	row.RecognitionInput = in.RecognitionInput
	row.NetMinor, row.GrossMinor = recognitionAmounts(in.RecognitionInput)
	row.OrganizationName = org
	row.IdentityName = name
	row.ProjectName = project
	if row.OriginalID != "" {
		if e = s.validateRecognitionReversal(ctx, tx, p, &row); e != nil {
			return empty, e
		}
	} else if row.SourceClaimID != "" {
		records, e := recognitionRows(ctx, tx, p.TenantID)
		if e != nil {
			return empty, e
		}
		if e = checkRecognitionCandidate(p, row, claim, records); e != nil {
			return empty, e
		}
	}
	if e = verifyRecognitionEvidence(ctx, tx, p, row); e != nil {
		return empty, e
	}
	row.Status = "DRAFT"
	row.Version++
	row.UpdatedAt = time.Now().UTC()
	raw, e := recognitionJSON(row)
	if e != nil {
		return empty, e
	}
	// 同阶段唯一Writer；数据库组织索引必须和新冻结内容一并更新。
	if _, e = tx.ExecContext(ctx, `UPDATE settlement_recognition SET organization_id=?,identity_id=?,payload=? WHERE tenant_id=? AND id=?`, row.OrganizationID, row.IdentityID, raw, p.TenantID, id); e != nil {
		return empty, e
	}
	if e = persistRecognition(ctx, tx, p, row, false, "EDITED"); e != nil {
		return empty, e
	}
	if e = tx.Commit(); e != nil {
		return empty, e
	}
	return row, nil
}
