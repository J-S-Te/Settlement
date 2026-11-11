package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

type CostItem struct {
	OrganizationID      string `json:"organization_id"`
	Applicant           string `json:"applicant_subject_id"`
	ClaimType           string `json:"claim_type"`
	ApprovedContentHash string `json:"approved_content_hash"`
	ItemID              string `json:"item_id"`
	AllocationKey       string `json:"allocation_key"`
	ProjectID           string `json:"project_id"`
	ServiceItemID       string `json:"service_item_id,omitempty"`
	ExpenseNature       string `json:"expense_nature"`
	FeeType             string `json:"fee_type"`
	OccurredDate        string `json:"occurred_date"`
	AmountMinor         int64  `json:"amount_minor"`
	EvidenceDigest      string `json:"evidence_digest"`
}
type CostAllocation struct {
	ProjectID     string  `json:"project_id"`
	ServiceItemID *string `json:"service_item_id"`
	FeeType       string  `json:"fee_type"`
	AmountMinor   string  `json:"amount_minor"`
}
type CostEffect struct {
	EffectID            string           `json:"effect_id"`
	PaymentRecordID     string           `json:"payment_record_id"`
	Kind                string           `json:"kind"`
	ReversesEffectID    string           `json:"reverses_effect_id,omitempty"`
	AmountMinor         string           `json:"amount_minor"`
	OccurredAt          string           `json:"occurred_at"`
	RecordedAt          string           `json:"recorded_at"`
	ApprovedContentHash string           `json:"approved_content_hash"`
	Allocations         []CostAllocation `json:"allocations"`
}
type CostClaim struct {
	ClaimID            string       `json:"claim_id"`
	ClaimNo            string       `json:"claim_no"`
	DocumentRevision   int64        `json:"document_revision"`
	ApprovalRound      int          `json:"approval_round"`
	ProjectionRevision int64        `json:"projection_revision"`
	Currency           string       `json:"currency"`
	Applicant          string       `json:"applicant_subject_id"`
	OrganizationID     string       `json:"organization_id"`
	ClaimType          string       `json:"claim_type"`
	Status             string       `json:"status"`
	UnknownReason      string       `json:"unknown_reason,omitempty"`
	Effects            []CostEffect `json:"effects"`
	FrozenItems        []CostItem   `json:"frozen_items"`
}
type CostSnapshot struct {
	SchemaVersion     int         `json:"schema_version"`
	SourceApplication string      `json:"source_application"`
	SourceEnvironment string      `json:"source_environment"`
	TenantID          string      `json:"tenant_id"`
	SnapshotAt        string      `json:"snapshot_at"`
	Complete          bool        `json:"complete"`
	Claims            []CostClaim `json:"claims"`
}
type ReimbursementCostSource interface {
	ReadCosts(context.Context, string, string) (CostSnapshot, error)
}
type HTTPReimbursementCosts struct {
	endpoint, tenant, environment string
	client                        *http.Client
}

// NewReimbursementCosts 装配独立机器凭据和短超时成本读取，禁止重定向泄露令牌。
// 参数：endpoint/tokenURL为受控地址，clientID/secret为专属凭据，tenant/environment为当前绑定。
// 返回值：真实HTTP来源；URL或身份缺失返回错误。
func NewReimbursementCosts(endpoint, tokenURL, clientID, secret, tenant, environment string) (*HTTPReimbursementCosts, error) {
	for _, raw := range []string{endpoint, tokenURL} {
		u, e := url.Parse(raw)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" {
			return nil, ErrInvalid
		}
	}
	if clientID == "" || secret == "" || tenant == "" || environment == "" {
		return nil, ErrInvalid
	}
	transport := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, transport)
	cfg := clientcredentials.Config{ClientID: clientID, ClientSecret: secret, TokenURL: tokenURL, Scopes: []string{"reimbursement.cost_candidate.read"}}
	client := cfg.Client(ctx)
	client.Timeout = 10 * time.Second
	client.CheckRedirect = transport.CheckRedirect
	return &HTTPReimbursementCosts{endpoint: endpoint, tenant: tenant, environment: environment, client: client}, nil
}

// ReadCosts 获取并整体复核完整快照，不把错误或半份来源转成可用零值。
// 参数：ctx控制超时，tenant须匹配装配身份，claimID可精确重验单据。
// 返回值：已校验的完整来源；远端失败或协议漂移返回依赖错误。
func (c *HTTPReimbursementCosts) ReadCosts(ctx context.Context, tenant, claimID string) (CostSnapshot, error) {
	var snapshot CostSnapshot
	if c == nil || tenant != c.tenant {
		return snapshot, ErrDependency
	}
	endpoint := c.endpoint
	if claimID != "" {
		endpoint += "?claim_id=" + url.QueryEscape(claimID)
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if e != nil {
		return snapshot, ErrDependency
	}
	response, e := c.client.Do(req)
	if e != nil {
		return snapshot, ErrDependency
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return snapshot, ErrDependency
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if e != nil || len(raw) > 8<<20 {
		return snapshot, ErrDependency
	}
	var envelope struct {
		Code string       `json:"code"`
		Data CostSnapshot `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code != "OK" {
		return snapshot, ErrDependency
	}
	snapshot = envelope.Data
	if validateCostSnapshot(snapshot, tenant, c.environment, claimID) != nil {
		return CostSnapshot{}, ErrDependency
	}
	return snapshot, nil
}

// validateCostSnapshot 核对唯一单据/效果、全额冲正、逐项金额与快照完整性。
// 参数：s为整个来源，tenant/environment为可信绑定，claimID为可选精确筛选。
// 返回值：合法无错误，协议不完整或金额不守恒返回依赖错误。
func validateCostSnapshot(s CostSnapshot, tenant, environment, claimID string) error {
	at, e := time.Parse(time.RFC3339Nano, s.SnapshotAt)
	if e != nil || at.After(time.Now().Add(time.Minute)) || s.SchemaVersion != 1 || !s.Complete || s.TenantID != tenant || s.SourceEnvironment != environment || s.SourceApplication != "reimbursement" || s.Claims == nil || len(s.Claims) > 1000 {
		return ErrDependency
	}
	seen := map[string]bool{}
	count := 0
	for _, claim := range s.Claims {
		if claim.ClaimID == "" || claim.DocumentRevision <= 0 || claim.ApprovalRound <= 0 || claim.ProjectionRevision <= 0 || seen[claim.ClaimID] || claimID != "" && claim.ClaimID != claimID || claim.Currency != "CNY" || claim.OrganizationID == "" || claim.Applicant == "" || claim.Status != "KNOWN" && claim.Status != "UNKNOWN" || len(claim.Effects) == 0 || len(claim.Effects) > 2000 {
			return ErrDependency
		}
		seen[claim.ClaimID] = true
		count += len(claim.FrozenItems)
		if count > 10000 {
			return ErrDependency
		}
		if err := validateCostClaim(claim); err != nil {
			return err
		}
	}
	return nil
}

// validateCostClaim 复验不可变付款与逐条全额冲正，避免项目副本或同额新效果重复。
// 参数：claim为一张权威完整付款历史。
// 返回值：金额与冻结费用守恒才返回无错误。
func validateCostClaim(claim CostClaim) error {
	if _, err := costClaimNet(claim); err != nil {
		return err
	}
	effects := map[string]CostEffect{}
	payments := map[string]bool{}
	reversed := map[string]bool{}
	for _, effect := range claim.Effects {
		value, e := strconv.ParseInt(effect.AmountMinor, 10, 64)
		if e != nil || strconv.FormatInt(value, 10) != effect.AmountMinor || value == 0 || value > RecognitionMaximum || value < -RecognitionMaximum || effect.EffectID == "" || effects[effect.EffectID].EffectID != "" || effect.PaymentRecordID == "" || len(effect.ApprovedContentHash) != 64 {
			return ErrDependency
		}
		for _, raw := range []string{effect.OccurredAt, effect.RecordedAt} {
			if _, e = time.Parse(time.RFC3339, raw); e != nil {
				return ErrDependency
			}
		}
		var sum int64
		keys := map[string]bool{}
		for _, allocation := range effect.Allocations {
			n, err := strconv.ParseInt(allocation.AmountMinor, 10, 64)
			key := costAllocationKey(allocation)
			if err != nil || n == 0 || n > RecognitionMaximum || n < -RecognitionMaximum || value > 0 && n < 0 || value < 0 && n > 0 || keys[key] || allocation.ProjectID == "" || allocation.FeeType == "" || sum+n > RecognitionMaximum || sum+n < -RecognitionMaximum {
				return ErrDependency
			}
			keys[key] = true
			sum += n
		}
		if sum != value {
			return ErrDependency
		}
		if effect.Kind == "PAYMENT_RECORDED" {
			if value <= 0 || effect.ReversesEffectID != "" || payments[effect.PaymentRecordID] {
				return ErrDependency
			}
			payments[effect.PaymentRecordID] = true
		} else if effect.Kind != "PAYMENT_FULLY_REVERSED" || value >= 0 || effect.ReversesEffectID == "" {
			return ErrDependency
		}
		effects[effect.EffectID] = effect
	}
	for _, effect := range claim.Effects {
		if effect.Kind != "PAYMENT_FULLY_REVERSED" {
			continue
		}
		original := effects[effect.ReversesEffectID]
		if original.Kind != "PAYMENT_RECORDED" || reversed[original.EffectID] || original.PaymentRecordID != effect.PaymentRecordID || effect.AmountMinor != "-"+original.AmountMinor || effect.ApprovedContentHash != original.ApprovedContentHash || len(effect.Allocations) != len(original.Allocations) {
			return ErrDependency
		}
		amounts := map[string]string{}
		for _, row := range original.Allocations {
			amounts[costAllocationKey(row)] = row.AmountMinor
		}
		for _, row := range effect.Allocations {
			if row.AmountMinor != "-"+amounts[costAllocationKey(row)] {
				return ErrDependency
			}
		}
		reversed[original.EffectID] = true
	}
	items := map[string]bool{}
	group := map[string]map[string]int64{}
	for _, row := range claim.FrozenItems {
		rawIdentity, _ := json.Marshal([]string{row.ProjectID, row.ServiceItemID})
		allocationDigest := sha256.Sum256(rawIdentity)
		if row.AllocationKey != hex.EncodeToString(allocationDigest[:]) {
			return ErrDependency
		}
		key := row.ApprovedContentHash + ":" + row.ItemID + ":" + row.AllocationKey
		if row.OrganizationID == "" || row.Applicant == "" || row.ItemID == "" || len(row.AllocationKey) != 64 || row.ProjectID == "" || row.AmountMinor <= 0 || row.AmountMinor > RecognitionMaximum || (row.ExpenseNature != "ACTUAL" && row.ExpenseNature != "PREPAYMENT") || items[key] || len(row.EvidenceDigest) != 64 {
			return ErrDependency
		}
		if _, e := time.Parse("2006-01-02", row.OccurredDate); e != nil {
			return ErrDependency
		}
		items[key] = true
		service := row.ServiceItemID
		a := CostAllocation{ProjectID: row.ProjectID, ServiceItemID: &service, FeeType: row.FeeType}
		allocation := costAllocationKey(a)
		if group[row.ApprovedContentHash] == nil {
			group[row.ApprovedContentHash] = map[string]int64{}
		}
		if group[row.ApprovedContentHash][allocation] > RecognitionMaximum-row.AmountMinor {
			return ErrDependency
		}
		group[row.ApprovedContentHash][allocation] += row.AmountMinor
	}
	return validateCostFrozenGroups(claim, group)
}

// costAllocationKey 保留付款效果中的项目/服务项/费用类别分组以核对守恒。
// 参数：allocation为来源分摊。
// 返回值：无歧义且只用于服务端比较的分组键。
func costAllocationKey(allocation CostAllocation) string {
	item := ""
	if allocation.ServiceItemID != nil {
		item = *allocation.ServiceItemID
	}
	raw, _ := json.Marshal([]string{allocation.ProjectID, item, allocation.FeeType})
	return string(raw)
}

// costClaimByID 定位完整快照中的精确单据，禁止不存在时回退到其它单据。
// 参数：snapshot为已验证来源，id为精确单据ID。
// 返回值：完整单据；未找到返回中文依赖错误。
func costClaimByID(snapshot CostSnapshot, id string) (CostClaim, error) {
	for _, claim := range snapshot.Claims {
		if claim.ClaimID == id {
			return claim, nil
		}
	}
	return CostClaim{}, fmt.Errorf("%w: 报销候选单据不存在", ErrDependency)
}

// costStableKey 生成费用条目与分摊的稳定身份，不以重新付款制造新的费用。
// 参数：item/allocation为审批冻结身份。
// 返回值：无歧义稳定分组字符串。
func costStableKey(item, allocation string) string {
	raw, _ := json.Marshal([]string{item, allocation})
	return strings.TrimSpace(string(raw))
}

// costClaimNet 使用大整数核对完整付款历史净额，避免大量退款重付累加溢出。
// 参数：claim为完整不可变付款与冲正历史。
// 返回值：JS安全范围内非负净付款额；超限或非法金额返回来源错误。
func costClaimNet(claim CostClaim) (int64, error) {
	total := new(big.Int)
	for _, effect := range claim.Effects {
		amount, ok := new(big.Int).SetString(effect.AmountMinor, 10)
		if !ok {
			return 0, ErrDependency
		}
		total.Add(total, amount)
	}
	if total.Sign() < 0 || !total.IsInt64() || total.Int64() > RecognitionMaximum {
		return 0, ErrDependency
	}
	return total.Int64(), nil
}

// validateCostFrozenGroups 按批准摘要双向核对冻结费用与付款分摊，不以整单UNKNOWN跳过已知效果。
// 参数：claim为已校验付款历史，groups为按批准hash及项目服务项费用类型归组的冻结整数分。
// 返回值：所有有冻结证据的效果均完整守恒才成功；仅整组缺证据的UNKNOWN效果可保留未知。
func validateCostFrozenGroups(claim CostClaim, groups map[string]map[string]int64) error {
	referenced := map[string]bool{}
	for _, effect := range claim.Effects {
		if effect.Kind != "PAYMENT_RECORDED" {
			continue
		}
		hash := effect.ApprovedContentHash
		referenced[hash] = true
		frozen, known := groups[hash]
		if !known {
			if claim.Status == "KNOWN" {
				return ErrDependency
			}
			continue
		}
		if len(frozen) != len(effect.Allocations) {
			return ErrDependency
		}
		var total int64
		for _, allocation := range effect.Allocations {
			expected, e := strconv.ParseInt(allocation.AmountMinor, 10, 64)
			actual, exists := frozen[costAllocationKey(allocation)]
			if e != nil || !exists || actual != expected || actual > RecognitionMaximum-total {
				return ErrDependency
			}
			total += actual
		}
		expected, e := strconv.ParseInt(effect.AmountMinor, 10, 64)
		if e != nil || total != expected {
			return ErrDependency
		}
	}
	for hash := range groups {
		if !referenced[hash] {
			return ErrDependency
		}
	}
	return nil
}
