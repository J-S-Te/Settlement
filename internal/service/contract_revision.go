package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"
)

// 合同财务修订的人工复核原因。值域写在迁移注释中，后续扩展无需 DDL。
const (
	// contractRevisionHoldPlanDiff 表示修订条款与已有财务事实冲突，
	// 未实现部分已自动重排，含回款/核销/开票/预占的差异保持原值待人工处理。
	contractRevisionHoldPlanDiff = "PLAN_DIFF_IMPLEMENTED_FACTS"
	// contractRevisionHoldTermTotal 表示新条款合计不等于新合同金额，
	// 整次修订 fail-closed，不写快照、不改计划。
	contractRevisionHoldTermTotal = "TERM_TOTAL_MISMATCH"
)

// 修订差异类型。只描述事实，不表达处理结论。
const (
	contractRevisionDiffAmount        = "AMOUNT_CHANGED"
	contractRevisionDiffDue           = "DUE_DATE_CHANGED"
	contractRevisionDiffAmountAndDue  = "AMOUNT_AND_DUE_DATE_CHANGED"
	contractRevisionDiffRemoved       = "REMOVED"
	contractRevisionHoldStatusPending = "PENDING_REVIEW"
	// contractRevisionHoldStatusResolved 表示人工已按修订差异完成调整与红冲。
	contractRevisionHoldStatusResolved = "RESOLVED"
)

// revisionImplementedFacts 是判断计划能否自动改写的唯一事实来源：
// 任何一项非零都代表已经发生或已经预占的财务事实。
type revisionImplementedFacts struct {
	ReceivedAllocated string `json:"received_allocated"`
	WriteOff          string `json:"write_off"`
	Invoiced          string `json:"invoiced"`
	Reserved          string `json:"reserved"`
}

// any 判断是否已有任何已实现或已预占的财务事实。
// 参数：无。
// 返回值：存在非零事实时返回真。
func (f revisionImplementedFacts) any() bool {
	for _, value := range []string{f.ReceivedAllocated, f.WriteOff, f.Invoiced, f.Reserved} {
		if r, ok := new(big.Rat).SetString(strings.TrimSpace(value)); ok && r.Sign() != 0 {
			return true
		}
	}
	return false
}

// contractRevisionDiff 是单个分期的三方差异（旧条款、新条款、已实现事实）。
type contractRevisionDiff struct {
	InstallmentNo    int                      `json:"installment_no"`
	Kind             string                   `json:"kind"`
	OldDueDate       string                   `json:"old_due_date,omitempty"`
	NewDueDate       string                   `json:"new_due_date,omitempty"`
	OldAmount        string                   `json:"old_amount,omitempty"`
	NewAmount        string                   `json:"new_amount,omitempty"`
	ImplementedFacts revisionImplementedFacts `json:"implemented_facts"`
	References       int                      `json:"references,omitempty"`
}

// contractRevisionDetail 是工单存储的完整差异明细，供人工按分期核对。
type contractRevisionDetail struct {
	FromVersion       int                    `json:"from_version"`
	ToVersion         int                    `json:"to_version"`
	OldContractAmount string                 `json:"old_contract_amount"`
	NewContractAmount string                 `json:"new_contract_amount"`
	OldTermTotal      string                 `json:"old_term_total,omitempty"`
	NewTermTotal      string                 `json:"new_term_total,omitempty"`
	NewTerms          []PaymentTerm          `json:"new_terms"`
	Diffs             []contractRevisionDiff `json:"diffs"`
}

// contractRevisionPlan 是锁定的旧计划及其已实现财务事实。
type contractRevisionPlan struct {
	ID                 string
	InstallmentNo      int
	DueDate            string
	PlannedAmount      string
	ConfirmationStatus string
	ReceivableID       string
	References         int
	Facts              revisionImplementedFacts
}

// ContractRevisionHold 是供财务查看的人工复核工单只读视图。
type ContractRevisionHold struct {
	ID                 string          `json:"id"`
	SourceContractID   string          `json:"source_contract_id"`
	SourceContractNo   string          `json:"source_contract_no"`
	ContractSnapshotID string          `json:"contract_snapshot_id,omitempty"`
	RevisionEventID    string          `json:"revision_event_id"`
	FromVersion        int             `json:"from_contract_version"`
	ToVersion          int             `json:"to_contract_version"`
	Reason             string          `json:"hold_reason"`
	Diff               json.RawMessage `json:"diff"`
	Status             string          `json:"status"`
	ResolutionReason   string          `json:"resolution_reason"`
	ResolvedBy         string          `json:"resolved_by"`
	CreatedAt          string          `json:"created_at"`
}

// normalizeContractTerms 按首版相同口径校验并规范化新条款。
// 参数：terms 为事件中的付款计划。
// 返回值：规范化条款、条款合计及参数错误；合计不参与比较，由调用方决定是否入人工复核。
func normalizeContractTerms(terms []PaymentTerm) ([]PaymentTerm, *big.Rat, error) {
	total := new(big.Rat)
	seen := make(map[int]bool, len(terms))
	normalized := make([]PaymentTerm, 0, len(terms))
	for _, term := range terms {
		value, err := amount(term.Amount)
		if err != nil {
			return nil, nil, err
		}
		if term.InstallmentNo < 1 {
			return nil, nil, fmt.Errorf("%w: installment_no must be positive", ErrInvalid)
		}
		if seen[term.InstallmentNo] {
			return nil, nil, fmt.Errorf("%w: installment_no must be unique", ErrInvalid)
		}
		seen[term.InstallmentNo] = true
		due := strings.TrimSpace(term.DueDate)
		if _, err := time.Parse("2006-01-02", due); err != nil {
			return nil, nil, fmt.Errorf("%w: due_date must be YYYY-MM-DD", ErrInvalid)
		}
		part, _ := new(big.Rat).SetString(value)
		total.Add(total, part)
		normalized = append(normalized, PaymentTerm{InstallmentNo: term.InstallmentNo, DueDate: due, Amount: value})
	}
	return normalized, total, nil
}

// loadContractRevisionPlans 一次锁定读取旧计划及其已实现事实。
// 参数：ctx 为上下文，tx 为入站事务，tenant 为可信租户，snapshot 为旧快照。
// 返回值：按分期排序的计划事实，数据库错误原样返回。
func loadContractRevisionPlans(ctx context.Context, tx *sql.Tx, tenant, snapshot string) ([]contractRevisionPlan, error) {
	// FOR UPDATE 锁住计划与应收行，防止人工调整在修订读取与写入之间丢失更新。
	rows, err := tx.QueryContext(ctx, `SELECT rp.id,rp.installment_no,DATE_FORMAT(rp.due_date,'%Y-%m-%d'),rp.planned_amount,rp.confirmation_status,COALESCE(r.id,''),COALESCE(r.received_allocated_amount,0),COALESCE(r.write_off_amount,0),COALESCE(r.invoiced_amount,0),COALESCE((SELECT SUM(a.reserved_amount-a.invoiced_amount-a.released_amount) FROM settlement_invoice_request_allocation a WHERE a.tenant_id=rp.tenant_id AND a.receivable_id=r.id AND a.status='RESERVED'),0),(SELECT COUNT(*) FROM settlement_dunning_case dc WHERE dc.tenant_id=rp.tenant_id AND (dc.plan_id=rp.id OR dc.receivable_id=r.id))+(SELECT COUNT(*) FROM settlement_invoice_request_allocation a2 WHERE a2.tenant_id=rp.tenant_id AND a2.receivable_id=r.id) FROM settlement_receivable_plan rp LEFT JOIN settlement_receivable r ON r.tenant_id=rp.tenant_id AND r.receivable_plan_id=rp.id WHERE rp.tenant_id=? AND rp.contract_snapshot_id=? ORDER BY rp.installment_no FOR UPDATE`, tenant, snapshot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := []contractRevisionPlan{}
	for rows.Next() {
		var plan contractRevisionPlan
		if err = rows.Scan(&plan.ID, &plan.InstallmentNo, &plan.DueDate, &plan.PlannedAmount, &plan.ConfirmationStatus, &plan.ReceivableID, &plan.Facts.ReceivedAllocated, &plan.Facts.WriteOff, &plan.Facts.Invoiced, &plan.Facts.Reserved, &plan.References); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

// revisionDiff 组装单分期差异事实。
// 参数：plan 为旧计划，term 为新条款，removed 表示新条款已无该分期。
// 返回值：差异明细。
func revisionDiff(plan contractRevisionPlan, term PaymentTerm, removed bool) contractRevisionDiff {
	diff := contractRevisionDiff{
		InstallmentNo:    plan.InstallmentNo,
		OldDueDate:       plan.DueDate,
		OldAmount:        plan.PlannedAmount,
		ImplementedFacts: plan.Facts,
		References:       plan.References,
	}
	if removed {
		diff.Kind = contractRevisionDiffRemoved
		return diff
	}
	diff.NewDueDate, diff.NewAmount = term.DueDate, term.Amount
	amountChanged := plan.PlannedAmount != term.Amount
	dueChanged := plan.DueDate != term.DueDate
	switch {
	case amountChanged && dueChanged:
		diff.Kind = contractRevisionDiffAmountAndDue
	case amountChanged:
		diff.Kind = contractRevisionDiffAmount
	default:
		diff.Kind = contractRevisionDiffDue
	}
	return diff
}

// applyContractRevision 在同一入站事务内接收高于当前版本的合同财务修订。
// 规则：新条款合计必须等于新合同金额；未实现计划直接重排或改额/删建；
// 已实现事实的差异只登记人工复核工单，绝不自动改写。版本回退由调用方拒绝。
// 参数：ctx 为上下文，tx 为入站事务，source 为来源应用，event 为修订事件，
// contractAmount 为新合同金额，currentVersion 为当前已接收版本，inboxID 为已登记入站事件。
// 返回值：是否登记人工复核、工单 ID 及协议或存储错误。
func (s *Service) applyContractRevision(ctx context.Context, tx *sql.Tx, source string, event ContractEvent, contractAmount string, currentVersion int, inboxID string) (bool, string, error) {
	terms, termTotal, err := normalizeContractTerms(event.Contract.PaymentTerms)
	if err != nil {
		return false, "", err
	}
	// 修订由 settlement_contract_stream 行锁串行化，旧快照只被修订分支降级，
	// 因此这里先快照读、再锁计划、后写快照，与 AdjustPlan“先计划后快照”的
	// 锁顺序一致，避免人工调整与修订互相死锁。
	var oldSnapshotID, oldAmount, migrationState string
	err = tx.QueryRowContext(ctx, `SELECT id,contract_amount,migration_state FROM settlement_contract_snapshot WHERE tenant_id=? AND source_application=? AND source_contract_id=? AND source_contract_version=? AND financial_status='EFFECTIVE'`, event.TenantID, source, event.Contract.ID, currentVersion).Scan(&oldSnapshotID, &oldAmount, &migrationState)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", fmt.Errorf("%w: current effective financial snapshot is missing", ErrConflict)
	}
	if err != nil {
		return false, "", err
	}
	// 导入对账 HOLD 的历史合同不得被自动修订，必须由财务先完成对账放行。
	if migrationState != "RELEASED" {
		return false, "", fmt.Errorf("%w: contract financial revision is blocked while import reconciliation is pending", ErrImportBlocked)
	}
	detail := contractRevisionDetail{
		FromVersion:       currentVersion,
		ToVersion:         event.Contract.Version,
		OldContractAmount: oldAmount,
		NewContractAmount: contractAmount,
		NewTermTotal:      termTotal.FloatString(2),
		NewTerms:          terms,
		Diffs:             []contractRevisionDiff{},
	}
	// 新条款合计不等于新合同金额时整次修订 fail-closed：
	// 不写新快照、不推进合同版本、不改任何计划，只登记人工复核并返回成功接收。
	if termTotal.Cmp(add(contractAmount)) != 0 {
		holdID, err := s.registerContractRevisionHold(ctx, tx, source, event, currentVersion, contractRevisionHoldTermTotal, "", detail)
		if err != nil {
			return false, "", err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_inbox_event SET status='HELD',processed_at=UTC_TIMESTAMP(3) WHERE id=?`, inboxID); err != nil {
			return false, "", err
		}
		return true, holdID, nil
	}
	plans, err := loadContractRevisionPlans(ctx, tx, event.TenantID, oldSnapshotID)
	if err != nil {
		return false, "", err
	}
	oldTermTotal := new(big.Rat)
	for _, plan := range plans {
		oldTermTotal.Add(oldTermTotal, add(plan.PlannedAmount))
	}
	detail.OldTermTotal = oldTermTotal.FloatString(2)

	termByNo := make(map[int]PaymentTerm, len(terms))
	for _, term := range terms {
		termByNo[term.InstallmentNo] = term
	}
	adjustByID := map[string]PaymentTerm{}
	removeIDs := map[string]bool{}
	seen := make(map[int]bool, len(plans))
	for _, plan := range plans {
		term, matched := termByNo[plan.InstallmentNo]
		if !matched {
			// 新条款已删除该分期：有已实现事实或仍被催收/开票记录引用时必须人工处理。
			if plan.Facts.any() || plan.References > 0 {
				detail.Diffs = append(detail.Diffs, revisionDiff(plan, PaymentTerm{}, true))
				continue
			}
			removeIDs[plan.ID] = true
			continue
		}
		seen[plan.InstallmentNo] = true
		changed := plan.PlannedAmount != term.Amount || plan.DueDate != term.DueDate
		if plan.Facts.any() {
			// 含已实现事实的差异只登记工单，计划保留原金额与到期日。
			if changed {
				detail.Diffs = append(detail.Diffs, revisionDiff(plan, term, false))
			}
			continue
		}
		if changed {
			adjustByID[plan.ID] = term
		}
	}
	createdTerms := []PaymentTerm{}
	for _, term := range terms {
		if !seen[term.InstallmentNo] {
			createdTerms = append(createdTerms, term)
		}
	}

	newSnapshotID := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,project_id,project_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'EFFECTIVE',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newSnapshotID, event.TenantID, source, event.EventID, event.Contract.ID, event.Contract.No, event.Contract.Version, event.Contract.CustomerID, event.Contract.CustomerName, event.Contract.ProjectID, event.Contract.ProjectName, strings.ToUpper(event.Contract.Currency), contractAmount, event.OccurredAt)
	if err != nil {
		if mysqlDuplicate(err) {
			return false, "", ErrConflict
		}
		return false, "", err
	}
	// 旧快照只做状态降级（VARCHAR 值域扩展，无 DDL），历史行保持可追溯。
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_contract_snapshot SET financial_status='SUPERSEDED',updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND source_application=? AND source_contract_id=? AND source_contract_version=? AND financial_status='EFFECTIVE'`, event.TenantID, source, event.Contract.ID, currentVersion); err != nil {
		return false, "", err
	}
	for _, plan := range plans {
		if removeIDs[plan.ID] {
			if _, err = tx.ExecContext(ctx, `DELETE FROM settlement_receivable WHERE tenant_id=? AND receivable_plan_id=?`, event.TenantID, plan.ID); err != nil {
				return false, "", err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM settlement_receivable_plan WHERE tenant_id=? AND id=?`, event.TenantID, plan.ID); err != nil {
				return false, "", err
			}
			if err = auditContractRevisionPlan(ctx, tx, event, currentVersion, plan, "REMOVED", PaymentTerm{}); err != nil {
				return false, "", err
			}
			continue
		}
		term, adjust := adjustByID[plan.ID]
		if !adjust {
			if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable_plan SET contract_snapshot_id=?,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=?`, newSnapshotID, event.TenantID, plan.ID); err != nil {
				return false, "", err
			}
			// 计划迁到新版本基线后，其应收也必须指向同一快照，
			// 否则开票余额与快照级查询会出现计划/应收版本错位。
			if plan.ReceivableID != "" {
				if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET contract_snapshot_id=?,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=?`, newSnapshotID, event.TenantID, plan.ReceivableID); err != nil {
					return false, "", err
				}
			}
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable_plan SET contract_snapshot_id=?,due_date=?,planned_amount=?,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=?`, newSnapshotID, term.DueDate, term.Amount, event.TenantID, plan.ID); err != nil {
			return false, "", err
		}
		if plan.ConfirmationStatus == "CONFIRMED" {
			if _, err = tx.ExecContext(ctx, `UPDATE settlement_receivable SET contract_snapshot_id=?,due_date=?,original_amount=?,open_amount=?-received_allocated_amount-write_off_amount,collection_status=CASE WHEN ?=received_allocated_amount+write_off_amount THEN 'SETTLED' WHEN received_allocated_amount>0 THEN 'PARTIALLY_SETTLED' ELSE 'UNPAID' END,version=version+1,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND receivable_plan_id=?`, newSnapshotID, term.DueDate, term.Amount, term.Amount, term.Amount, event.TenantID, plan.ID); err != nil {
				return false, "", err
			}
			if err = auditContractRevisionPlan(ctx, tx, event, currentVersion, plan, "ADJUSTED", term); err != nil {
				return false, "", err
			}
		}
	}
	for _, term := range createdTerms {
		if _, err = tx.ExecContext(ctx, `INSERT INTO settlement_receivable_plan(id,tenant_id,contract_snapshot_id,installment_no,due_date,planned_amount,generation_source,confirmation_status,created_at,updated_at) VALUES(?,?,?,?,?,?,'CONTRACT_EVENT','PENDING_CONFIRMATION',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, newID(), event.TenantID, newSnapshotID, term.InstallmentNo, term.DueDate, term.Amount); err != nil {
			return false, "", err
		}
	}
	held := len(detail.Diffs) > 0
	holdID := ""
	if held {
		holdID, err = s.registerContractRevisionHold(ctx, tx, source, event, currentVersion, contractRevisionHoldPlanDiff, newSnapshotID, detail)
		if err != nil {
			return false, "", err
		}
	}
	status := "COMPLETED"
	if held {
		status = "HELD"
	}
	if err = outbox(ctx, tx, event.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_CONTRACT_REVISION_APPLIED", "contract_snapshot", newSnapshotID, map[string]any{
		"actor_id":              "SYSTEM",
		"source_application":    source,
		"source_contract_id":    event.Contract.ID,
		"source_contract_no":    event.Contract.No,
		"revision_event_id":     event.EventID,
		"from_contract_version": currentVersion,
		"to_contract_version":   event.Contract.Version,
		"old_contract_amount":   oldAmount,
		"new_contract_amount":   contractAmount,
		"adjusted_plans":        len(adjustByID),
		"removed_plans":         len(removeIDs),
		"created_plans":         len(createdTerms),
		"held_diffs":            len(detail.Diffs),
		"hold_id":               holdID,
		"result":                "SUCCESS",
		"risk_level":            "MEDIUM",
	}); err != nil {
		return false, "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_inbox_event SET status=?,processed_at=UTC_TIMESTAMP(3) WHERE id=?`, status, inboxID); err != nil {
		return false, "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_contract_stream SET current_version=?,updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND source_application=? AND source_contract_id=?`, event.Contract.Version, event.TenantID, source, event.Contract.ID); err != nil {
		return false, "", err
	}
	return held, holdID, nil
}

// registerContractRevisionHold 登记人工复核工单并写高风险审计。
// 参数：ctx 为上下文，tx 为入站事务，source 为来源应用，event 为修订事件，
// fromVersion 为修订前版本，reason 为工单原因，snapshot 为新快照（可为空），detail 为差异明细。
// 返回值：工单 ID；同事件同原因已存在时返回既有工单，不重复审计。
func (s *Service) registerContractRevisionHold(ctx context.Context, tx *sql.Tx, source string, event ContractEvent, fromVersion int, reason, snapshot string, detail contractRevisionDetail) (string, error) {
	body, err := json.Marshal(detail)
	if err != nil {
		return "", err
	}
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_contract_revision_hold(id,tenant_id,source_application,source_contract_id,source_contract_no,contract_snapshot_id,revision_event_id,from_contract_version,to_contract_version,hold_reason,diff_json,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, event.TenantID, source, event.Contract.ID, event.Contract.No, snapshot, event.EventID, fromVersion, event.Contract.Version, reason, body, contractRevisionHoldStatusPending)
	if err != nil {
		if mysqlDuplicate(err) {
			var existing string
			lookup := tx.QueryRowContext(ctx, `SELECT id FROM settlement_contract_revision_hold WHERE tenant_id=? AND source_application=? AND revision_event_id=? AND hold_reason=?`, event.TenantID, source, event.EventID, reason).Scan(&existing)
			if lookup != nil {
				return "", lookup
			}
			return existing, nil
		}
		return "", err
	}
	if err = outbox(ctx, tx, event.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_CONTRACT_REVISION_HELD", "contract_revision_hold", id, map[string]any{
		"actor_id":              "SYSTEM",
		"source_application":    source,
		"source_contract_id":    event.Contract.ID,
		"source_contract_no":    event.Contract.No,
		"revision_event_id":     event.EventID,
		"from_contract_version": fromVersion,
		"to_contract_version":   event.Contract.Version,
		"hold_reason":           reason,
		"held_diffs":            len(detail.Diffs),
		"result":                "SUCCESS",
		"risk_level":            "HIGH",
	}); err != nil {
		return "", err
	}
	return id, nil
}

// auditContractRevisionPlan 为自动改额或删除的计划写 SETTLEMENT_PLAN_ADJUSTED 审计。
// 参数：ctx 为上下文，tx 为入站事务，event 为修订事件，fromVersion 为修订前版本，
// plan 为旧计划，action 为 ADJUSTED/REMOVED，term 为新条款（删除时为空）。
// 返回值：审计入队或存储错误。
func auditContractRevisionPlan(ctx context.Context, tx *sql.Tx, event ContractEvent, fromVersion int, plan contractRevisionPlan, action string, term PaymentTerm) error {
	payload := map[string]any{
		"actor_id":              "SYSTEM",
		"action":                action,
		"revision_event_id":     event.EventID,
		"source_contract_id":    event.Contract.ID,
		"from_contract_version": fromVersion,
		"to_contract_version":   event.Contract.Version,
		"installment_no":        plan.InstallmentNo,
		"old_due_date":          plan.DueDate,
		"old_amount":            plan.PlannedAmount,
		"reason":                "contract financial revision",
		"result":                "SUCCESS",
		"risk_level":            "HIGH",
	}
	if term.InstallmentNo != 0 {
		payload["due_date"] = term.DueDate
		payload["amount"] = term.Amount
	}
	return outbox(ctx, tx, event.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_PLAN_ADJUSTED", "receivable_plan", plan.ID, payload)
}

// ContractRevisionHoldQuery 表达人工复核工单列表的有界分页与可选筛选。
// 参数：Status 只接受 PENDING_REVIEW/RESOLVED；ContractRef 同时匹配源合同ID与合同编号。
type ContractRevisionHoldQuery struct {
	Limit       int
	Offset      int
	Status      string
	ContractRef string
}

// contractRevisionHoldQuery 校验工单查询边界，保留无参数的既有调用。
// 参数：inputs 为可选的唯一查询条件。
// 返回值：规范化查询；分页越界、筛选过长或状态未知时返回 ErrInvalid。
func contractRevisionHoldQuery(inputs ...ContractRevisionHoldQuery) (ContractRevisionHoldQuery, error) {
	query := ContractRevisionHoldQuery{Limit: 100}
	if len(inputs) > 1 {
		return query, ErrInvalid
	}
	if len(inputs) == 1 {
		query = inputs[0]
	}
	query.Status = strings.TrimSpace(query.Status)
	query.ContractRef = strings.TrimSpace(query.ContractRef)
	if query.Limit == 0 {
		query.Limit = 100
	}
	if query.Limit < 1 || query.Limit > 200 || query.Offset < 0 || query.Offset > 100000 || textTooLong(query.ContractRef, 128) {
		return query, ErrInvalid
	}
	if query.Status != "" && query.Status != contractRevisionHoldStatusPending && query.Status != contractRevisionHoldStatusResolved {
		return query, ErrInvalid
	}
	return query, nil
}

// ListContractRevisionHolds 返回可见范围内的合同修订人工复核工单。
// 参数：ctx 为上下文，p 为可信主体。
// 返回值：按创建时间倒序的有界工单列表；无权返回禁止。
func (s *Service) ListContractRevisionHolds(ctx context.Context, p Principal) ([]ContractRevisionHold, error) {
	return s.ListContractRevisionHoldsPage(ctx, p, ContractRevisionHoldQuery{})
}

// ListContractRevisionHoldsPage 返回按状态/合同筛选的工单分页。
// 参数：ctx 为上下文，p 为可信主体，query 为状态、合同引用与分页条件。
// 返回值：当前页工单；无权、筛选越界或持久化失败返回错误。
func (s *Service) ListContractRevisionHoldsPage(ctx context.Context, p Principal, query ContractRevisionHoldQuery) ([]ContractRevisionHold, error) {
	// 只认专用只读权限，与 HTTP 层 settlement.revision.read 同源，避免前端被迫叠加
	// 两个权限。目录中销售与项目助理同样持有该权限，因此下面的责任范围谓词仍按
	// 原角色生成，不因换权限而放宽数据范围。
	if !p.Allows("settlement.revision.read") {
		return nil, ErrForbidden
	}
	filter, err := contractRevisionHoldQuery(query)
	if err != nil {
		return nil, err
	}
	clause, scopeArgs := p.ContractPredicate("settlement.revision.read", "cs")
	statement := `SELECT cs.id,cs.source_contract_id,cs.source_contract_no,COALESCE(cs.contract_snapshot_id,''),cs.revision_event_id,cs.from_contract_version,cs.to_contract_version,cs.hold_reason,cs.diff_json,cs.status,cs.resolution_reason,cs.resolved_by,DATE_FORMAT(cs.created_at,'%Y-%m-%dT%H:%i:%s.%fZ') FROM settlement_contract_revision_hold cs WHERE cs.tenant_id=? AND ` + clause
	args := append([]any{p.TenantID}, scopeArgs...)
	if filter.Status != "" {
		statement += " AND cs.status=?"
		args = append(args, filter.Status)
	}
	if filter.ContractRef != "" {
		statement += " AND (cs.source_contract_id=? OR cs.source_contract_no=?)"
		args = append(args, filter.ContractRef, filter.ContractRef)
	}
	statement += " ORDER BY cs.created_at DESC,cs.id DESC LIMIT ? OFFSET ?"
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.DB.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ContractRevisionHold{}
	for rows.Next() {
		var item ContractRevisionHold
		var diff []byte
		if err = rows.Scan(&item.ID, &item.SourceContractID, &item.SourceContractNo, &item.ContractSnapshotID, &item.RevisionEventID, &item.FromVersion, &item.ToVersion, &item.Reason, &diff, &item.Status, &item.ResolutionReason, &item.ResolvedBy, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Diff = json.RawMessage(diff)
		items = append(items, item)
	}
	return items, rows.Err()
}

// ResolveContractRevisionHold 在人工完成 AdjustPlan/CreateManualPlan/红冲组合后关闭工单。
// 参数：ctx 为上下文，p 为可信主体，id 为工单，reason 为处理说明。
// 返回值：成功无错误；无权、状态不符、范围不可见或存储失败返回错误。
func (s *Service) ResolveContractRevisionHold(ctx context.Context, p Principal, id, reason string) error {
	// 关单只认专用动作权限，与 HTTP 层 settlement.revision.resolve 同源。
	// 该权限在目录中仅授予系统管理员与财务经理，不会因此扩大可关单角色；
	// 下方事务内的合同范围校验保持原有强度。
	if !p.Allows("settlement.revision.resolve") {
		return ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	// 长度按字符（rune）计，与前端“5–500 字”一致；按字节会放过 2 个汉字。
	if strings.TrimSpace(id) == "" || utf8.RuneCountInString(reason) < 5 || textTooLong(reason, 500) {
		return fmt.Errorf("%w: hold id and resolution reason (5-500 characters) are required", ErrInvalid)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, snapshot string
	err = tx.QueryRowContext(ctx, `SELECT status,COALESCE(contract_snapshot_id,'') FROM settlement_contract_revision_hold WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, id).Scan(&status, &snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == contractRevisionHoldStatusResolved {
		// 同一关单原因重放按幂等成功处理：不重复改状态、不重复写审计；
		// 已用不同原因关单时拒绝覆盖人工处理说明。
		var resolvedReason string
		if err = tx.QueryRowContext(ctx, `SELECT resolution_reason FROM settlement_contract_revision_hold WHERE tenant_id=? AND id=?`, p.TenantID, id).Scan(&resolvedReason); err != nil {
			return err
		}
		if strings.TrimSpace(resolvedReason) == reason {
			return nil
		}
		return ErrConflict
	}
	if status != contractRevisionHoldStatusPending {
		return ErrConflict
	}
	if snapshot != "" {
		// 范围校验仍是事务内、按快照责任行锁定：仅换用专用动作权限，
		// 不跳过任何合同可见性判断。
		if err = s.RequireContractAccessTx(ctx, tx, p, "settlement.revision.resolve", snapshot); err != nil {
			return err
		}
		// 关单闸门：新快照下不能还有待确认计划或在途开票申请；
		// 已开票红冲等已实现事实仍由财务在关单说明中确认（见核对报告）。
		var blocking int
		if err = tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM settlement_receivable_plan WHERE tenant_id=? AND contract_snapshot_id=? AND confirmation_status<>'CONFIRMED')
			+ (SELECT COUNT(*) FROM settlement_invoice_request WHERE tenant_id=? AND contract_snapshot_id=? AND status IN ('SUBMITTED','SPECIAL_REVIEW','ISSUE_PENDING','ISSUE_UNKNOWN'))`,
			p.TenantID, snapshot, p.TenantID, snapshot).Scan(&blocking); err != nil {
			return err
		}
		if blocking > 0 {
			return fmt.Errorf("%w: 存在未确认的应收计划或在途开票申请，不能关闭修订工单", ErrConflict)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE settlement_contract_revision_hold SET status='RESOLVED',resolution_reason=?,resolved_by=?,resolved_at=UTC_TIMESTAMP(3),updated_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND id=? AND status='PENDING_REVIEW'`, reason, p.UserID, p.TenantID, id); err != nil {
		return err
	}
	if err = outbox(ctx, tx, p.TenantID, "PLATFORM_AUDIT", "SETTLEMENT_CONTRACT_REVISION_HOLD_RESOLVED", "contract_revision_hold", id, map[string]any{
		"actor_id":          p.UserID,
		"resolution":        reason,
		"contract_snapshot": snapshot,
		"result":            "SUCCESS",
		"risk_level":        "HIGH",
	}); err != nil {
		return err
	}
	return tx.Commit()
}
