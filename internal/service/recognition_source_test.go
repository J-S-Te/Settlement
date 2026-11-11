package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// TestCostCandidateAllocationIdentityCannotDrift 验证同项目服务项的稳定分摊身份必须由规范内容确定。
// 参数：t为测试上下文。
// 返回值：无，来源更换分摊键以制造新费用额度时失败。
func TestCostCandidateAllocationIdentityCannotDrift(t *testing.T) {
	raw, _ := json.Marshal([]string{"project", ""})
	digest := sha256.Sum256(raw)
	key := hex.EncodeToString(digest[:])
	hash := strings.Repeat("a", 64)
	claim := CostClaim{Status: "KNOWN", Effects: []CostEffect{{EffectID: "e", PaymentRecordID: "p", Kind: "PAYMENT_RECORDED", AmountMinor: "40", OccurredAt: "2026-11-01T00:00:00Z", RecordedAt: "2026-11-01T00:00:00Z", ApprovedContentHash: hash, Allocations: []CostAllocation{{ProjectID: "project", FeeType: "TRAVEL", AmountMinor: "40"}}}}, FrozenItems: []CostItem{{OrganizationID: "org", Applicant: "app", ApprovedContentHash: hash, ItemID: "item", AllocationKey: key, ProjectID: "project", ExpenseNature: "ACTUAL", FeeType: "TRAVEL", OccurredDate: "2026-10-30", AmountMinor: 40, EvidenceDigest: hash}}}
	if e := validateCostClaim(claim); e != nil {
		t.Fatal(e)
	}
	claim.FrozenItems[0].AllocationKey = strings.Repeat("b", 64)
	if e := validateCostClaim(claim); e == nil {
		t.Fatal("accepted allocation identity drift")
	}
}

// TestRecognitionCandidateSeparatesSourceEnvironments 验证同单据标识在不同来源环境不共享额度。
// 参数：t为测试上下文。
// 返回值：无，旧环境的确认错误消耗当前环境额度时失败。
func TestRecognitionCandidateSeparatesSourceEnvironments(t *testing.T) {
	input := RecognitionInput{SourceSystem: "reimbursement", SourceEnvironment: "prod", SourceClaimID: "claim", SourceEffectID: "effect", SourceItemID: "item", SourceAllocationKey: "allocation", ProjectID: "project", OccurredDate: "2026-11-01", Currency: "CNY"}
	row := Recognition{RecognitionInput: input, NetMinor: 40, GrossMinor: 40}
	old := row
	old.ID = "old-environment"
	old.Status = "CONFIRMED"
	old.SourceEnvironment = "local"
	claim := CostClaim{ClaimID: "claim", Status: "KNOWN", Currency: "CNY", Effects: []CostEffect{{EffectID: "effect", Kind: "PAYMENT_RECORDED", AmountMinor: "40", ApprovedContentHash: "hash"}}, FrozenItems: []CostItem{{ApprovedContentHash: "hash", ItemID: "item", AllocationKey: "allocation", ProjectID: "project", OccurredDate: "2026-11-01", AmountMinor: 40, ExpenseNature: "ACTUAL"}}}
	if e := checkRecognitionCandidate(Principal{}, row, claim, []Recognition{old}); e != nil {
		t.Fatalf("foreign source environment consumed current balance: %v", e)
	}
}
