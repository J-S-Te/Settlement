package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// validImportFactIdentity 限定精确比较的来源键，拒绝尾空格与控制字符歧义。
// 参数：value为原始来源键，maximum为UTF-8字节上限。
// 返回值：可按二进制稳定存储和比较时为true。
func validImportFactIdentity(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

var ErrImportBlocked = fmt.Errorf("historical financial fact type is blocked")

type ExpectedImportFact struct {
	SourceSystem   string `json:"source_system"`
	FactType       string `json:"fact_type"`
	ExternalFactID string `json:"external_fact_id"`
}

type ContractImportCommand struct {
	ExpectedFacts           []ExpectedImportFact `json:"expected_facts"`
	EventID                 string               `json:"event_id"`
	EventType               string               `json:"event_type"`
	SchemaVersion           int                  `json:"schema_version"`
	Phase                   string               `json:"phase"`
	TenantID                string               `json:"tenant_id"`
	OriginImportItemID      string               `json:"origin_import_item_id"`
	SourceRevision          uint64               `json:"source_revision"`
	PolicyDigest            string               `json:"policy_digest"`
	ReleaseVersion          uint64               `json:"release_version"`
	ImportMode              string               `json:"import_mode"`
	OccurredAt              time.Time            `json:"occurred_at"`
	Contract                json.RawMessage      `json:"contract"`
	ReconciliationID        string               `json:"reconciliation_id,omitempty"`
	ExpectedSnapshotVersion uint64               `json:"expected_snapshot_version,omitempty"`
	ExpectedOwnerVersion    uint64               `json:"expected_owner_version,omitempty"`
}

type ImportReceipt struct {
	EventID            string `json:"event_id"`
	ContractSnapshotID string `json:"contract_snapshot_id"`
	Status             string `json:"status"`
	MigrationState     string `json:"migration_state"`
	Duplicate          bool   `json:"duplicate"`
}

// validateImportCommand 检查新协议身份、版本和固定阶段，禁止普通v1借道导入。
// 参数：tenant为验签租户，phase为当前端点要求，in为不可变源命令。
// 返回值：非法报文返回参数错误，否则无错误。
func validateImportCommand(tenant, phase string, in ContractImportCommand) error {
	if tenant == "" || in.TenantID != tenant || in.EventType != "contract.import_financial.v1" || in.SchemaVersion != 1 || in.Phase != phase {
		return ErrInvalid
	}
	if in.SourceRevision == 0 || in.ReleaseVersion == 0 || in.OccurredAt.IsZero() || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(in.PolicyDigest) {
		return ErrInvalid
	}
	if in.ImportMode != "CONTINUE_LIVE" && in.ImportMode != "HISTORICAL_CLOSED" {
		return ErrInvalid
	}
	for _, v := range []string{in.EventID, in.OriginImportItemID} {
		if v == "" || strings.TrimSpace(v) != v || len(v) > 128 {
			return ErrInvalid
		}
	}
	if phase == "PREPARE" {
		if in.ExpectedFacts == nil || len(in.ExpectedFacts) > 100000 {
			return ErrInvalid
		}
		seen := map[string]bool{}
		for _, fact := range in.ExpectedFacts {
			if fact.FactType != "RECEIPT" && fact.FactType != "ALLOCATION" {
				return ErrImportBlocked
			}
			if !validImportFactIdentity(fact.SourceSystem, 64) || !validImportFactIdentity(fact.ExternalFactID, 128) {
				return ErrInvalid
			}
			key := fact.SourceSystem + "\x00" + fact.FactType + "\x00" + fact.ExternalFactID
			if seen[key] {
				return ErrInvalid
			}
			seen[key] = true
		}
	}
	return nil
}

// PrepareContractImport 原子建立HOLD财务基线，回执只说明暂存完成。
// 参数：ctx为事务上下文，tenant为机器可信租户，in为稳定PREPARE命令。
// 返回值：本地快照回执，协议、幂等冲突或数据库错误。
func (s *Service) PrepareContractImport(ctx context.Context, tenant string, in ContractImportCommand) (ImportReceipt, error) {
	result := ImportReceipt{EventID: in.EventID, Status: "FINANCE_PREPARED", MigrationState: "HOLD"}
	if err := validateImportCommand(tenant, "PREPARE", in); err != nil {
		return result, err
	}
	if !s.ContractImportEnabled {
		if s.DB == nil {
			return result, fmt.Errorf("%w: contract import is disabled", ErrForbidden)
		}
		var stored []byte
		err := s.DB.QueryRowContext(ctx, `SELECT request_hash FROM settlement_idempotency_record WHERE tenant_id=? AND command_type='IMPORT_PREPARE' AND idempotency_key=?`, tenant, in.EventID).Scan(&stored)
		if err == sql.ErrNoRows {
			return result, fmt.Errorf("%w: contract import is disabled", ErrForbidden)
		}
		if err != nil {
			return result, err
		}
		body, _ := json.Marshal(in)
		hash := sha256.Sum256(body)
		if !bytes.Equal(stored, hash[:]) {
			return result, ErrConflict
		}
	}
	var event ContractEvent
	event.EventID = in.EventID
	event.EventType = "contract.financial_effective.v1"
	event.TenantID = tenant
	event.OccurredAt = in.OccurredAt
	if err := json.Unmarshal(in.Contract, &event.Contract); err != nil || event.Contract.OwnerUserID == "" || len(in.EventID) > 32 {
		return result, ErrInvalid
	}
	created, err := s.ingestContract(ctx, "contract_management", tenant, event, &in)
	if err != nil {
		return result, err
	}
	err = s.DB.QueryRowContext(ctx, `SELECT id,migration_state FROM settlement_contract_snapshot WHERE tenant_id=? AND source_application='contract_management' AND source_event_id=?`, tenant, in.EventID).Scan(&result.ContractSnapshotID, &result.MigrationState)
	result.Duplicate = !created
	return result, err
}

// ImportFact 保存可重复生成的来源身份及核销关系，不能以本次循环序号充当事实ID。
type ImportFact struct {
	ContractSnapshotID    string `json:"contract_snapshot_id"`
	SourceSystem          string `json:"source_system"`
	FactType              string `json:"fact_type"`
	ExternalFactID        string `json:"external_fact_id"`
	OriginalReceiptFactID string `json:"original_receipt_fact_id,omitempty"`
	OriginalInstallmentNo int    `json:"original_installment_no,omitempty"`
	OriginalAllocationID  string `json:"original_allocation_id,omitempty"`
}

// lockImportWrite 在与RELEASE共用的快照锁内校验恢复能力并使旧对账版本失效。
// 参数：ctx为上下文，tx为当前业务事务，p为主体，snapshot为实际业务快照。
// 返回值：普通合同无副作用，HOLD缺财务恢复能力时拒绝，其余返回存储错误。
func lockImportWrite(ctx context.Context, tx *sql.Tx, p Principal, snapshot string) error {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT migration_state FROM settlement_contract_snapshot WHERE tenant_id=? AND id=?`, p.TenantID, snapshot).Scan(&state); err != nil {
		return err
	}
	// 普通记录永不从RELEASED回到HOLD，不增加普通发票和回款的锁争用。
	if state != "HOLD" {
		return nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT migration_state FROM settlement_contract_snapshot WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, snapshot).Scan(&state); err != nil {
		return err
	}
	if state != "HOLD" {
		return ErrConflict
	}
	if err := financeAccess(p, "settlement.import.reconcile"); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE settlement_contract_snapshot SET import_snapshot_version=import_snapshot_version+1 WHERE tenant_id=? AND id=?`, p.TenantID, snapshot)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settlement_contract_import SET state='PREPARED',reconciliation_id='' WHERE tenant_id=? AND contract_snapshot_id=?`, p.TenantID, snapshot)
	return err
}

// claimImportFact 把来源事实与业务ID写入同一事务，支持响应丢失和重启后恢复。
// 参数：ctx为上下文，tx为业务事务，p为主体，fact为来源身份，kind为事实类型，id为目标，body为内容。
// 返回值：同内容返回原ID及重复标识，同键异内容返回冲突；不支持事实明确拒绝。
func claimImportFact(ctx context.Context, tx *sql.Tx, p Principal, fact *ImportFact, kind, id string, body any) (string, bool, error) {
	if fact == nil {
		return id, false, nil
	}
	if fact.FactType != kind || fact.ContractSnapshotID == "" || !validImportFactIdentity(fact.SourceSystem, 64) || !validImportFactIdentity(fact.ExternalFactID, 128) {
		return "", false, ErrInvalid
	}
	if kind == "ALLOCATION" && (fact.OriginalReceiptFactID == "" || fact.OriginalInstallmentNo < 1 || fact.OriginalAllocationID == "") {
		return "", false, ErrInvalid
	}
	if err := financeAccess(p, "settlement.import.reconcile"); err != nil {
		return "", false, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", false, err
	}
	hash := sha256.Sum256(payload)
	var oldHash []byte
	var oldID string
	lookup := tx.QueryRowContext(ctx, `SELECT request_hash,target_business_id FROM settlement_import_fact WHERE tenant_id=? AND contract_snapshot_id=? AND source_system=? AND fact_type=? AND external_fact_id=?`, p.TenantID, fact.ContractSnapshotID, fact.SourceSystem, kind, fact.ExternalFactID).Scan(&oldHash, &oldID)
	if lookup == nil {
		if !bytes.Equal(oldHash, hash[:]) {
			return "", false, ErrConflict
		}
		return oldID, true, nil
	}
	if lookup != sql.ErrNoRows {
		return "", false, lookup
	}
	if err := lockImportWrite(ctx, tx, p, fact.ContractSnapshotID); err != nil {
		return "", false, err
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT migration_state FROM settlement_contract_snapshot WHERE tenant_id=? AND id=?`, p.TenantID, fact.ContractSnapshotID).Scan(&state); err != nil {
		return "", false, err
	}
	if state != "HOLD" {
		return "", false, ErrConflict
	}
	var expected []byte
	if err := tx.QueryRowContext(ctx, `SELECT expected_facts_json FROM settlement_contract_import WHERE tenant_id=? AND contract_snapshot_id=?`, p.TenantID, fact.ContractSnapshotID).Scan(&expected); err != nil {
		return "", false, err
	}
	var manifest []ExpectedImportFact
	if err := json.Unmarshal(expected, &manifest); err != nil {
		return "", false, err
	}
	declared := false
	for _, f := range manifest {
		if f.SourceSystem == fact.SourceSystem && f.FactType == fact.FactType && f.ExternalFactID == fact.ExternalFactID {
			declared = true
			break
		}
	}
	if !declared {
		return "", false, ErrImportBlocked
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settlement_import_fact(tenant_id,contract_snapshot_id,source_system,fact_type,external_fact_id,request_hash,target_business_id,created_at) VALUES(?,?,?,?,?,?,?,UTC_TIMESTAMP(3))`, p.TenantID, fact.ContractSnapshotID, fact.SourceSystem, kind, fact.ExternalFactID, hash[:], id)
	if err == nil {
		return id, false, nil
	}
	if !mysqlDuplicate(err) {
		return "", false, err
	}
	var old []byte
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT request_hash,target_business_id FROM settlement_import_fact WHERE tenant_id=? AND contract_snapshot_id=? AND source_system=? AND fact_type=? AND external_fact_id=? FOR UPDATE`, p.TenantID, fact.ContractSnapshotID, fact.SourceSystem, kind, fact.ExternalFactID).Scan(&old, &existing)
	if err != nil {
		return "", false, err
	}
	if !bytes.Equal(old, hash[:]) {
		return "", false, ErrConflict
	}
	return existing, true, nil
}

// importFactKey 从来源事实生成原生幂等键，不改变普通业务传入的请求键。
// 参数：fact为来源事实，fallback为普通业务键。
// 返回值：稳定的导入请求键或原键。
func importFactKey(fact *ImportFact, fallback string) string {
	if fact == nil {
		return fallback
	}
	value, _ := json.Marshal([]string{fact.ContractSnapshotID, fact.SourceSystem, fact.FactType, fact.ExternalFactID})
	sum := sha256.Sum256(value)
	return "IMPORT-" + hex.EncodeToString(sum[:])
}

// rejectImportedReceiptMutation 阻止缺少逐事实事务适配器的退款或差异改写暂存来源凭证。
// 参数：ctx为上下文，tx为业务事务，tenant为租户，receipt为凭证ID。
// 返回值：导入HOLD凭证返回明确阻塞，普通凭证无影响。
func rejectImportedReceiptMutation(ctx context.Context, tx *sql.Tx, tenant, receipt string) error {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settlement_import_fact f JOIN settlement_contract_snapshot cs ON cs.id=f.contract_snapshot_id AND cs.tenant_id=f.tenant_id WHERE f.tenant_id=? AND f.fact_type='RECEIPT' AND f.target_business_id=? AND cs.migration_state='HOLD'`, tenant, receipt).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrImportBlocked
	}
	return nil
}
