package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// recognitionLock 串行化同租户账本变更，统一保护冲回余额和跨付款费用上限。
// 参数：ctx为请求，tx为写事务，tenant为可信租户。
// 返回值：锁成功无错误；数据库失败原样返回。
func recognitionLock(ctx context.Context, tx *sql.Tx, tenant string) error {
	if _, e := tx.ExecContext(ctx, `INSERT IGNORE INTO settlement_recognition_lock(tenant_id,created_at) VALUES(?,UTC_TIMESTAMP(3))`, tenant); e != nil {
		return e
	}
	var id string
	return tx.QueryRowContext(ctx, `SELECT tenant_id FROM settlement_recognition_lock WHERE tenant_id=? FOR UPDATE`, tenant).Scan(&id)
}

// loadRecognition 从当前事务精确读取确认事实，不使用客户端租户。
// 参数：ctx/tx为事务上下文，tenant/id为精确身份。
// 返回值：真实记录；不可见或不存在返回不存在。
func loadRecognition(ctx context.Context, tx *sql.Tx, tenant, id string) (Recognition, error) {
	var row Recognition
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM settlement_recognition WHERE tenant_id=? AND id=?`, tenant, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrNotFound
	}
	if err != nil {
		return row, err
	}
	if json.Unmarshal(raw, &row) != nil || row.TenantID != tenant || row.ID != id {
		return Recognition{}, ErrDependency
	}
	return row, nil
}

// recognitionRows 读取事务内有界完整账本以核对稳定费用及冲回余额。
// 参数：ctx/tx为事务，tenant为可信租户。
// 返回值：完整记录，超过10000行明确失败，不截断后执行确认。
func recognitionRows(ctx context.Context, tx *sql.Tx, tenant string) ([]Recognition, error) {
	rows, e := tx.QueryContext(ctx, `SELECT payload FROM settlement_recognition WHERE tenant_id=? ORDER BY id LIMIT 10001`, tenant)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []Recognition{}
	for rows.Next() {
		var raw []byte
		var item Recognition
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if json.Unmarshal(raw, &item) != nil || item.TenantID != tenant {
			return nil, ErrDependency
		}
		result = append(result, item)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	if len(result) > 10000 {
		return nil, fmt.Errorf("%w: 确认账本超过完整快照上限", ErrDependency)
	}
	return result, nil
}

// persistRecognition 保存当前状态及不可变版本事件，并在同事务生成审计Outbox。
// 参数：ctx/tx为事务，p为操作者，row为完整记录，create标识首次写入，action为受控动作。
// 返回值：持久化或审计失败返回错误，调用方必须回滚。
func persistRecognition(ctx context.Context, tx *sql.Tx, p Principal, row Recognition, create bool, action string) error {
	raw, e := recognitionJSON(row)
	if e != nil {
		return e
	}
	if create {
		var key any
		if row.SourceSystem == "manual" && row.OriginalID == "" {
			source, _ := json.Marshal([]string{row.SourceSystem, row.SourceEnvironment, row.Kind, row.SourceFactID})
			hash := sha256.Sum256(source)
			key = hex.EncodeToString(hash[:])
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO settlement_recognition(id,tenant_id,organization_id,identity_id,status,source_claim_id,original_id,source_key,payload,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, row.ID, row.TenantID, row.OrganizationID, row.IdentityID, row.Status, row.SourceClaimID, row.OriginalID, key, raw, row.CreatedAt, row.UpdatedAt)
	} else {
		_, e = tx.ExecContext(ctx, `UPDATE settlement_recognition SET status=?,payload=?,updated_at=? WHERE tenant_id=? AND id=?`, row.Status, raw, row.UpdatedAt, row.TenantID, row.ID)
	}
	if e != nil {
		if mysqlDuplicate(e) {
			return ErrConflict
		}
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO settlement_recognition_event(id,tenant_id,recognition_id,version,actor_id,action,payload,created_at) VALUES(?,?,?,?,?,?,?,?)`, newID(), row.TenantID, row.ID, row.Version, p.UserID, action, raw, row.UpdatedAt)
	if e != nil {
		return e
	}
	return outbox(ctx, tx, row.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_RECOGNITION_"+action, "recognition", row.ID, map[string]any{"actor_id": p.UserID, "version": row.Version, "organization_id": row.OrganizationID, "status": row.Status, "original_id": row.OriginalID, "net_minor": row.NetMinor, "currency": row.Currency})
}

// CreateRecognition 登记真实依据草稿；已确认事实更正只能新建关联冲回。
// 参数：ctx/p为当前授权上下文，key为幂等键，in为草稿完整内容。
// 返回值：草稿或同内容回放，来源、范围、引用或金额错误时拒绝。
func (s *Service) CreateRecognition(ctx context.Context, p Principal, key string, in RecognitionInput) (Recognition, error) {
	var empty Recognition
	if !recognitionAllowed(p, "settlement.recognition.record", in.OrganizationID) {
		return empty, ErrForbidden
	}
	if e := validateRecognition(in, time.Now()); e != nil {
		return empty, e
	}
	if s.RecognitionEnvironment != "" && in.SourceEnvironment != s.RecognitionEnvironment {
		return empty, ErrInvalid
	}
	key, e := nativeIdempotencyKey(key)
	if e != nil {
		return empty, e
	}
	orgName, personName, e := s.verifyRecognitionDirectory(ctx, p, in)
	if e != nil {
		return empty, e
	}
	projectName, e := s.verifyRecognitionProject(ctx, p, in)
	if e != nil {
		return empty, e
	}
	var claim CostClaim
	if in.SourceClaimID != "" && in.OriginalID == "" {
		claim, e = s.recognitionClaim(ctx, p, in)
		if e != nil {
			return empty, e
		}
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback()
	if e = recognitionLock(ctx, tx, p.TenantID); e != nil {
		return empty, e
	}
	id, replay, e := claimIdempotency(ctx, tx, p.TenantID, "RECOGNITION_CREATE", key, newID(), struct {
		Actor string
		Input RecognitionInput
	}{p.UserID, in})
	if e != nil {
		return empty, e
	}
	if replay {
		row, e := loadRecognition(ctx, tx, p.TenantID, id)
		if e == nil && !recognitionAllowed(p, "settlement.recognition.record", row.OrganizationID) {
			e = ErrForbidden
		}
		return row, e
	}
	net, gross := recognitionAmounts(in)
	now := time.Now().UTC()
	row := Recognition{RecognitionInput: in, ID: id, TenantID: p.TenantID, OrganizationName: orgName, IdentityName: personName, ProjectName: projectName, NetMinor: net, GrossMinor: gross, Status: "DRAFT", Version: 1, CreatedBy: p.UserID, CreatedAt: now, UpdatedAt: now}
	if in.OriginalID != "" {
		if e = s.validateRecognitionReversal(ctx, tx, p, &row); e != nil {
			return empty, e
		}
	} else if in.SourceClaimID != "" {
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
	if e = persistRecognition(ctx, tx, p, row, true, "CREATED"); e != nil {
		return empty, e
	}
	if e = tx.Commit(); e != nil {
		return empty, e
	}
	return row, nil
}

// verifyRecognitionProject 核对同租户权威项目、服务项及所选合同关系。
// 参数：ctx/p为可信身份，in为显式关联引用。
// 返回值：权威项目名称；依赖不可用或关系错误时拒绝。
func (s *Service) verifyRecognitionProject(ctx context.Context, p Principal, in RecognitionInput) (string, error) {
	if in.ProjectID == "" {
		return "", nil
	}
	if s.ProjectReferences == nil {
		return "", ErrDependency
	}
	ref, e := s.ProjectReferences.Resolve(ctx, p.TenantID, in.ProjectID)
	if e != nil {
		return "", ErrDependency
	}
	if ref.OwnerOrgID == "" || !recognitionAllowed(p, "settlement.recognition.read", ref.OwnerOrgID) {
		return "", ErrForbidden
	}
	if !ref.Active || ref.TenantID != p.TenantID || ref.ProjectID != in.ProjectID {
		return "", ErrInvalid
	}
	if in.ServiceItemID != "" {
		found := false
		for _, id := range ref.ServiceItemIDs {
			if id == in.ServiceItemID {
				found = true
			}
		}
		if !found {
			return "", ErrInvalid
		}
	}
	if in.ContractSnapshotID != "" {
		var contract string
		e = s.DB.QueryRowContext(ctx, `SELECT source_contract_id FROM settlement_contract_snapshot WHERE tenant_id=? AND id=? AND migration_state='RELEASED'`, p.TenantID, in.ContractSnapshotID).Scan(&contract)
		if e != nil || contract != ref.ContractID {
			return "", ErrInvalid
		}
	}
	return ref.Name, nil
}

// verifyRecognitionEvidence 验证凭证确实存在于同租户账本，禁止以任意字符串假充真实凭证。
// 参数：ctx/tx为事务，p为可信主体，row为待登记事实。
// 返回值：凭证及合同关系合法才无错误。
func verifyRecognitionEvidence(ctx context.Context, tx *sql.Tx, p Principal, row Recognition) error {
	if strings.HasPrefix(row.EvidenceReference, "file:") {
		var org, status string
		e := tx.QueryRowContext(ctx, `SELECT organization_id,status FROM settlement_recognition_evidence WHERE tenant_id=? AND id=?`, p.TenantID, strings.TrimPrefix(row.EvidenceReference, "file:")).Scan(&org, &status)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrInvalid
		}
		if e != nil {
			return e
		}
		if org != row.OrganizationID || status != "READY" {
			return fmt.Errorf("%w: 证据尚未完成上传绑定或组织不一致", ErrInvalid)
		}
		return nil
	}
	if err := financeAccess(p, "settlement.voucher.manage"); err != nil {
		return err
	}
	var voucher, contract, currency string
	e := tx.QueryRowContext(ctx, `SELECT v.id,r.contract_snapshot_id,w.currency FROM settlement_accounting_voucher v JOIN settlement_receipt_allocation w ON w.tenant_id=v.tenant_id AND w.id=v.writeoff_id JOIN settlement_receivable r ON r.tenant_id=w.tenant_id AND r.id=w.receivable_id WHERE v.tenant_id=? AND v.id=?`, p.TenantID, row.EvidenceReference).Scan(&voucher, &contract, &currency)
	if errors.Is(e, sql.ErrNoRows) {
		return fmt.Errorf("%w: 请选择同租户的真实结算凭证", ErrInvalid)
	}
	if e != nil {
		return e
	}
	if currency != row.Currency {
		return fmt.Errorf("%w: 凭证币种与确认币种不一致", ErrInvalid)
	}
	if row.ContractSnapshotID != "" && row.ContractSnapshotID != contract {
		return fmt.Errorf("%w: 凭证与所选合同不一致", ErrInvalid)
	}
	return nil
}

// validateRecognitionReversal 复制原确认归属及来源并校验当前可冲回金额，保留原事实不变。
// 参数：ctx/tx为事务，p为操作者，row为冲回草稿；金额及确认日为本次独立输入。
// 返回值：合法关联冲回或金额/范围冲突。
func (s *Service) validateRecognitionReversal(ctx context.Context, tx *sql.Tx, p Principal, row *Recognition) error {
	original, e := loadRecognition(ctx, tx, p.TenantID, row.OriginalID)
	if e != nil {
		return e
	}
	if !recognitionAllowed(p, "settlement.recognition.record", original.OrganizationID) {
		return ErrForbidden
	}
	if original.Status != "CONFIRMED" || original.OriginalID != "" || original.OrganizationID != row.OrganizationID || original.Currency != row.Currency || original.Kind != row.Kind || row.NetMinor > original.NetMinor || row.TaxMinor > original.TaxMinor {
		return ErrConflict
	}
	amount, tax, basis, date, evidence, note := row.AmountMinor, row.TaxMinor, row.TaxBasis, row.RecognitionDate, row.EvidenceReference, row.Note
	row.RecognitionInput = original.RecognitionInput
	row.OriginalID = original.ID
	row.AmountMinor = amount
	row.TaxMinor = tax
	row.TaxBasis = basis
	row.RecognitionDate = date
	row.EvidenceReference = evidence
	row.Note = note
	row.OrganizationName = original.OrganizationName
	row.IdentityName = original.IdentityName
	row.ProjectName = original.ProjectName
	records, e := recognitionRows(ctx, tx, p.TenantID)
	if e != nil {
		return e
	}
	return checkRecognitionReversal(*row, original, records)
}

// checkRecognitionReversal 防止累计净额或税额冲回超过原始确认额。
// 参数：row为本次冲回，original为不可变原确认，records为完整账本。
// 返回值：剩余额度足够才无错误。
func checkRecognitionReversal(row, original Recognition, records []Recognition) error {
	net, tax := original.NetMinor, original.TaxMinor
	for _, item := range records {
		if item.OriginalID == original.ID && item.Status == "CONFIRMED" {
			net -= item.NetMinor
			tax -= item.TaxMinor
		}
	}
	if row.NetMinor > net || row.TaxMinor > tax {
		return fmt.Errorf("%w: 冲回金额超过原确认剩余净额或税额", ErrConflict)
	}
	return nil
}

// TransitionRecognition 提交草稿、独立确认或退回，所有回放仍先复核当前权限和范围。
// 参数：ctx/p为当前身份，id为记录，action为submit/decision，key为幂等键，in为版本与决定。
// 返回值：真实状态；自审、旧版本、来源差异、超额或依赖失败拒绝。
func (s *Service) TransitionRecognition(ctx context.Context, p Principal, id, action, key string, in RecognitionDecision) (Recognition, error) {
	var empty Recognition
	permission := "settlement.recognition.record"
	if action == "decision" {
		permission = "settlement.recognition.review"
	}
	if !recognitionAllowed(p, permission, "") {
		return empty, ErrForbidden
	}
	before, e := s.GetRecognition(ctx, p, id, permission)
	if e != nil {
		return empty, e
	}
	if action == "decision" && (before.CreatedBy == p.UserID || before.SubmittedBy == p.UserID) {
		return empty, ErrForbidden
	}
	var claim CostClaim
	if before.SourceClaimID != "" && before.OriginalID == "" {
		claim, e = s.recognitionClaim(ctx, p, before.RecognitionInput)
		if e != nil {
			return empty, e
		}
	}
	if action == "decision" && in.Decision == "CONFIRMED" && before.OriginalID == "" {
		if _, _, e = s.verifyRecognitionDirectory(ctx, p, before.RecognitionInput); e != nil {
			return empty, e
		}
		if _, e = s.verifyRecognitionProject(ctx, p, before.RecognitionInput); e != nil {
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
	if !recognitionAllowed(p, permission, row.OrganizationID) {
		return empty, ErrForbidden
	}
	_, replay, e := claimIdempotency(ctx, tx, p.TenantID, "RECOGNITION_"+strings.ToUpper(action), key, id, struct {
		Actor, ID string
		Input     RecognitionDecision
	}{p.UserID, id, in})
	if e != nil {
		return empty, e
	}
	if replay {
		return row, nil
	}
	if row.Version != in.ExpectedVersion {
		return empty, ErrConflict
	}
	if e = applyRecognitionTransition(p, &row, action, in); e != nil {
		return empty, e
	}
	if row.Status == "SUBMITTED" {
		if e = verifyRecognitionEvidence(ctx, tx, p, row); e != nil {
			return empty, e
		}
	}
	if row.Status == "CONFIRMED" {
		records, e := recognitionRows(ctx, tx, p.TenantID)
		if e != nil {
			return empty, e
		}
		if row.OriginalID != "" {
			original, e := loadRecognition(ctx, tx, p.TenantID, row.OriginalID)
			if e != nil {
				return empty, e
			}
			if e = checkRecognitionReversal(row, original, records); e != nil {
				return empty, e
			}
		} else if row.SourceClaimID != "" {
			if e = checkRecognitionCandidate(p, row, claim, records); e != nil {
				return empty, e
			}
		}
		if e = verifyRecognitionEvidence(ctx, tx, p, row); e != nil {
			return empty, e
		}
	}
	row.Version++
	row.UpdatedAt = time.Now().UTC()
	if e = persistRecognition(ctx, tx, p, row, false, strings.ToUpper(action)); e != nil {
		return empty, e
	}
	if e = tx.Commit(); e != nil {
		return empty, e
	}
	return row, nil
}

// applyRecognitionTransition 实施最小财务状态机且强制登记、提交与复核职责分离。
// 参数：p为可信操作者，row为锁定记录，action/in为明确命令。
// 返回值：合法状态转换；自审、退回无原因或非法状态返回错误。
func applyRecognitionTransition(p Principal, row *Recognition, action string, in RecognitionDecision) error {
	if len(in.Note) > 2000 {
		return ErrInvalid
	}
	if action == "submit" {
		if row.CreatedBy != p.UserID {
			return ErrForbidden
		}
		if row.Status != "DRAFT" && row.Status != "RETURNED" {
			return ErrConflict
		}
		row.Status = "SUBMITTED"
		row.SubmittedBy = p.UserID
		row.ReviewNote = ""
		return nil
	}
	if action != "decision" || row.Status != "SUBMITTED" {
		return ErrConflict
	}
	if row.CreatedBy == p.UserID || row.SubmittedBy == p.UserID {
		return ErrForbidden
	}
	if in.Decision != "CONFIRMED" && in.Decision != "RETURNED" || in.Decision == "RETURNED" && strings.TrimSpace(in.Note) == "" {
		return ErrInvalid
	}
	row.Status = in.Decision
	row.ReviewedBy = p.UserID
	row.ReviewNote = in.Note
	now := time.Now().UTC()
	row.ReviewedAt = &now
	return nil
}

// GetRecognition 仅返回当前明确动作和冻结组织范围内的财务明细。
// 参数：ctx/p为可信上下文，id为记录，permission由后端固定为实际动作。
// 返回值：可见完整记录；其它租户或组织统一不存在。
func (s *Service) GetRecognition(ctx context.Context, p Principal, id, permission string) (Recognition, error) {
	var row Recognition
	var raw []byte
	if !recognitionAllowed(p, permission, "") {
		return row, ErrForbidden
	}
	e := s.DB.QueryRowContext(ctx, `SELECT payload FROM settlement_recognition WHERE tenant_id=? AND id=?`, p.TenantID, id).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return row, ErrNotFound
	}
	if e != nil {
		return row, e
	}
	if json.Unmarshal(raw, &row) != nil || row.TenantID != p.TenantID {
		return Recognition{}, ErrDependency
	}
	if !recognitionAllowed(p, permission, row.OrganizationID) {
		return Recognition{}, ErrNotFound
	}
	return row, nil
}

// ListRecognitions 在数据库分页之前按可信角色范围过滤，不向窄范围泄漏租户总数。
// 参数：ctx/p为身份，limit/offset为有界真实分页。
// 返回值：当前页明细；缺少明确授权返回错误。
func (s *Service) ListRecognitions(ctx context.Context, p Principal, limit, offset int) ([]Recognition, error) {
	if !recognitionAllowed(p, "settlement.recognition.read", "") {
		return nil, ErrForbidden
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, ErrInvalid
	}
	clause, args := recognitionPredicate(p, "settlement.recognition.read")
	args = append([]any{p.TenantID}, args...)
	args = append(args, limit, offset)
	rows, e := s.DB.QueryContext(ctx, `SELECT payload FROM settlement_recognition WHERE tenant_id=? AND (`+clause+`) ORDER BY created_at DESC,id LIMIT ? OFFSET ?`, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Recognition{}
	for rows.Next() {
		var raw []byte
		var row Recognition
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if json.Unmarshal(raw, &row) != nil {
			return nil, ErrDependency
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// recognitionPredicate 为已有动作生成组织过滤，只有明确全租户范围才取消组织限制。
// 参数：p为可信主体，permission为当前实际动作。
// 返回值：固定SQL谓词与绑定参数，未授权返回恒假。
func recognitionPredicate(p Principal, permission string) (string, []any) {
	clauses := []string{}
	args := []any{}
	for _, role := range p.Roles {
		if !catalogRoleGrants[role][permission] {
			continue
		}
		for _, scope := range p.DataScopes {
			if scope.RoleCode != role {
				continue
			}
			switch scope.ScopeType {
			case "TENANT":
				return "1=1", nil
			case "ORG":
				if scope.ScopeID != "" {
					clauses = append(clauses, "organization_id=?")
					args = append(args, scope.ScopeID)
				}
			}
		}
	}
	if len(clauses) == 0 {
		return "1=0", nil
	}
	return strings.Join(clauses, " OR "), args
}

// RecognitionProjectReference 返回有权组织内的权威项目及服务项候选供财务选择。
// 参数：ctx/p为当前身份，id为项目业务引用。
// 返回值：最小权威引用；无权、失效或依赖未配置时拒绝。
func (s *Service) RecognitionProjectReference(ctx context.Context, p Principal, id string) (any, error) {
	if !recognitionAllowed(p, "settlement.recognition.read", "") {
		return nil, ErrForbidden
	}
	if id == "" || len(id) > 128 {
		return nil, ErrInvalid
	}
	if s.ProjectReferences == nil {
		return nil, ErrDependency
	}
	ref, e := s.ProjectReferences.Resolve(ctx, p.TenantID, id)
	if e != nil {
		return nil, ErrDependency
	}
	if ref.TenantID != p.TenantID || ref.ProjectID != id || !ref.Active || ref.OwnerOrgID == "" || !recognitionAllowed(p, "settlement.recognition.read", ref.OwnerOrgID) {
		return nil, ErrNotFound
	}
	return map[string]any{"project_id": ref.ProjectID, "name": ref.Name, "owner_org_id": ref.OwnerOrgID, "service_item_ids": ref.ServiceItemIDs, "version": ref.Version}, nil
}
