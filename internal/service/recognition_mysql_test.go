package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/projectreference"
)

type recognitionTestDirectory struct{}

// RecognitionPeople 提供隔离回归的受控目录边界，数据库账本使用真实MySQL。
// 参数：ctx为测试上下文，id为精确合成人员，keyword未使用。
// 返回值：唯一合成人员及其明确组织。
func (recognitionTestDirectory) RecognitionPeople(ctx context.Context, id, keyword string) ([]RecognitionPerson, error) {
	return []RecognitionPerson{{ID: id, Name: "合成人员", Organizations: []RecognitionOrganization{{ID: "org", Name: "合成组织"}}}}, nil
}

type recognitionTestProjects struct{}

// Resolve 提供隔离回归的权威引用边界，不代表真实项目机器HTTP验收。
// 参数：ctx/tenant为可信请求，id为合成项目。
// 返回值：同租户有效引用。
func (recognitionTestProjects) Resolve(ctx context.Context, tenant, id string) (projectreference.Reference, error) {
	return projectreference.Reference{TenantID: tenant, ProjectID: id, OwnerOrgID: "org", ContractID: "contract", CustomerID: "customer", Name: "合成项目", Active: true, Version: 1, ServiceItemIDs: []string{"service"}}, nil
}

type recognitionTestSource struct{ snapshot CostSnapshot }

// ReadCosts 返回隔离测试完整候选，用于复现退款与重付的状态变化。
// 参数：ctx/tenant/claim为业务查询边界。
// 返回值：当前合成候选副本。
func (source *recognitionTestSource) ReadCosts(ctx context.Context, tenant, claim string) (CostSnapshot, error) {
	raw, _ := json.Marshal(source.snapshot)
	var out CostSnapshot
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// recognitionTestPrincipal 构造目录显式授予且有精确组织范围的财务主体。
// 参数：tenant为合成租户，user为合成人员，role为实际目录角色。
// 返回值：不使用legacy兼容的可信测试主体。
func recognitionTestPrincipal(tenant, user, role string) Principal {
	return Principal{TenantID: tenant, UserID: user, IdentityID: user, CatalogVersion: "5", Roles: []string{role}, DataScopes: []DataScope{{RoleCode: role, ScopeType: "ORG", ScopeID: "org"}}, Permissions: map[string]bool{"settlement.recognition.read": true, "settlement.recognition.record": true, "settlement.recognition.review": true}}
}

// recognitionTestInput 生成含真实隔离凭证引用的最小完整确认输入。
// 参数：voucher为真实隔离凭证，fact为独立来源事实，kind/amount为业务金额。
// 返回值：未税明确、发生日与确认日分离的输入。
func recognitionTestInput(voucher, fact, kind string, amount int64) RecognitionInput {
	in := RecognitionInput{Kind: kind, OrganizationID: "org", OrganizationWitnessID: "witness", Currency: "CNY", AmountMinor: amount, TaxBasis: "EXCLUSIVE", OccurredDate: "2026-10-30", RecognitionDate: "2026-11-01", EvidenceReference: voucher, Note: "隔离合成业务依据", SourceSystem: "manual", SourceEnvironment: "local", SourceFactID: fact}
	if kind == "COST" {
		in.CostCategory = "OTHER"
	}
	return in
}

// TestRecognitionMySQLStateAndRefund 验证真实事务状态机、独立审核、冲回与退款重付冻结。
// 参数：t为测试上下文，仅使用明确授权的私有隔离DSN。
// 返回值：无，金额、幂等、范围或并发守恒失败即报错。
func TestRecognitionMySQLStateAndRefund(t *testing.T) {
	fixtures := map[string]RecognitionMetrics{}
	db := dashboardFinanceMySQL(t)
	ctx := context.Background()
	tenant := "recognition-" + newID()
	svc := &Service{DB: db, RecognitionDirectory: recognitionTestDirectory{}, ProjectReferences: recognitionTestProjects{}, RecognitionEnvironment: "local"}
	maker := recognitionTestPrincipal(tenant, "maker", "settlement_accountant")
	reviewer := recognitionTestPrincipal(tenant, "reviewer", "settlement_reviewer")
	evidenceReference := recognitionTestFileEvidence(t, svc, maker)
	incomeInput := recognitionTestInput(evidenceReference, "income", "INCOME", 100)
	incomeInput.TaxMinor = 6
	incomeInput.RecognitionDate = "2026-10-31"
	income, e := svc.CreateRecognition(ctx, maker, "income", incomeInput)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := svc.CreateRecognition(ctx, maker, "income", income.RecognitionInput)
	if e != nil || replay.ID != income.ID {
		t.Fatalf("replay=%+v err=%v", replay, e)
	}
	bad := income.RecognitionInput
	bad.AmountMinor = 101
	if _, e = svc.CreateRecognition(ctx, maker, "income", bad); !errors.Is(e, ErrConflict) {
		t.Fatalf("different replay=%v", e)
	}
	if _, e = svc.GetRecognition(ctx, recognitionTestPrincipal(tenant+"other", "maker", "settlement_accountant"), income.ID, "settlement.recognition.read"); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross tenant=%v", e)
	}
	scoped := maker
	scoped.DataScopes = []DataScope{{RoleCode: "settlement_accountant", ScopeType: "ORG", ScopeID: "other"}}
	if _, e = svc.GetRecognition(ctx, scoped, income.ID, "settlement.recognition.read"); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross organization=%v", e)
	}
	income, e = svc.TransitionRecognition(ctx, maker, income.ID, "submit", "submit-income", RecognitionDecision{ExpectedVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	self := recognitionTestPrincipal(tenant, "maker", "settlement_admin")
	if _, e = svc.TransitionRecognition(ctx, self, income.ID, "decision", "self", RecognitionDecision{ExpectedVersion: 2, Decision: "CONFIRMED"}); !errors.Is(e, ErrForbidden) {
		t.Fatalf("self review=%v", e)
	}
	income, e = svc.TransitionRecognition(ctx, reviewer, income.ID, "decision", "approve-income", RecognitionDecision{ExpectedVersion: 2, Decision: "CONFIRMED"})
	if e != nil {
		t.Fatal(e)
	}
	cost, e := svc.CreateRecognition(ctx, maker, "cost", recognitionTestInput(evidenceReference, "cost", "COST", 40))
	if e != nil {
		t.Fatal(e)
	}
	edit := cost.RecognitionInput
	edit.Note = "补充真实草稿依据"
	cost, e = svc.UpdateRecognition(ctx, maker, cost.ID, "edit-cost", RecognitionEdit{RecognitionInput: edit, ExpectedVersion: 1})
	if e != nil || cost.Version != 2 || cost.Note != edit.Note {
		t.Fatalf("draft edit=%+v err=%v", cost, e)
	}
	cost, e = svc.TransitionRecognition(ctx, maker, cost.ID, "submit", "submit-cost", RecognitionDecision{ExpectedVersion: 2})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = svc.TransitionRecognition(ctx, reviewer, cost.ID, "decision", "return-cost", RecognitionDecision{ExpectedVersion: 3, Decision: "RETURNED", Note: "请复核依据"}); e != nil {
		t.Fatal(e)
	}
	cost, e = svc.TransitionRecognition(ctx, maker, cost.ID, "submit", "resubmit-cost", RecognitionDecision{ExpectedVersion: 4})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"approve-cost-a", "approve-cost-b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			_, err := svc.TransitionRecognition(ctx, reviewer, cost.ID, "decision", key, RecognitionDecision{ExpectedVersion: 5, Decision: "CONFIRMED"})
			results <- err
		}(key)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent successes=%d", success)
	}
	metrics, e := svc.DashboardMetrics(ctx, tenant)
	if e != nil {
		t.Fatal(e)
	}
	var totalIncome, totalCost int64
	for _, row := range metrics.Rows {
		switch row.Kind {
		case "recognized_income":
			totalIncome += row.AmountMinor
		case "recognized_cost":
			totalCost += row.AmountMinor
		}
	}
	fixtures["income_100_cost_40_confirmed"] = metrics
	if totalIncome != 100 || totalCost != 40 {
		t.Fatalf("income=%d cost=%d", totalIncome, totalCost)
	}
	incomeReverse := income.RecognitionInput
	incomeReverse.OriginalID = income.ID
	incomeReverse.RecognitionDate = "2026-11-02"
	incomeReverse.Note = "跨期独立冲回收入"
	undo, e := svc.CreateRecognition(ctx, maker, "income-reverse", incomeReverse)
	if e != nil {
		t.Fatal(e)
	}
	undo, e = svc.TransitionRecognition(ctx, maker, undo.ID, "submit", "income-reverse-submit", RecognitionDecision{ExpectedVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	_, e = svc.TransitionRecognition(ctx, reviewer, undo.ID, "decision", "income-reverse-confirm", RecognitionDecision{ExpectedVersion: 2, Decision: "CONFIRMED"})
	if e != nil {
		t.Fatal(e)
	}
	metrics, e = svc.DashboardMetrics(ctx, tenant)
	if e != nil {
		t.Fatal(e)
	}
	totalIncome = 0
	tax := int64(0)
	positiveMonth, negativeMonth := "", ""
	for _, item := range metrics.Rows {
		if item.Kind == "recognized_income" {
			totalIncome += item.AmountMinor
			tax += item.TaxMinor
			if item.AmountMinor > 0 {
				positiveMonth = item.Period
			} else {
				negativeMonth = item.Period
			}
		}
	}
	if totalIncome != 0 || tax != 0 || positiveMonth != "2026-10" || negativeMonth != "2026-11" {
		t.Fatalf("cross-period income=%d tax=%d months=%s/%s", totalIncome, tax, positiveMonth, negativeMonth)
	}
	fixtures["income_cross_period_reversed"] = metrics
	recognitionTestRefund(t, svc, maker, reviewer, evidenceReference, tenant, fixtures)
	if path := os.Getenv("SETTLEMENT_METRIC_FIXTURE_PATH"); path != "" {
		raw, e := json.MarshalIndent(map[string]any{"source": "真实隔离MySQL回归产出；目录/项目/报销HTTP使用明确测试边界", "scenarios": fixtures}, "", "  ")
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
}

// recognitionTestRefund 验证同费用重付不产生新额度，独立冲回后才释放。
// 参数：t/svc为真实隔离账本，maker/reviewer为分离角色，voucher/tenant为隔离资源。
// 返回值：无，未处置冲正放行或超过稳定费用限额即失败。
func recognitionTestRefund(t *testing.T, svc *Service, maker, reviewer Principal, voucher, tenant string, fixtures map[string]RecognitionMetrics) {
	t.Helper()
	ctx := context.Background()
	hash := strings.Repeat("a", 64)
	allocationBytes, _ := json.Marshal([]string{"project", ""})
	allocationDigest := sha256.Sum256(allocationBytes)
	allocationKey := hex.EncodeToString(allocationDigest[:])
	effect := CostEffect{EffectID: "effect-1", PaymentRecordID: "payment-1", Kind: "PAYMENT_RECORDED", AmountMinor: "50", OccurredAt: "2026-11-01T00:00:00Z", RecordedAt: "2026-11-01T00:01:00Z", ApprovedContentHash: hash, Allocations: []CostAllocation{{ProjectID: "project", FeeType: "TRAVEL", AmountMinor: "50"}}}
	source := &recognitionTestSource{snapshot: CostSnapshot{SchemaVersion: 1, SourceApplication: "reimbursement", SourceEnvironment: "local", TenantID: tenant, SnapshotAt: time.Now().UTC().Format(time.RFC3339Nano), Complete: true, Claims: []CostClaim{{ClaimID: "claim", ClaimNo: "QA", DocumentRevision: 3, ApprovalRound: 1, ProjectionRevision: 2, ClaimType: "TRAVEL", Currency: "CNY", Applicant: "applicant", OrganizationID: "org", Status: "KNOWN", Effects: []CostEffect{effect}, FrozenItems: []CostItem{{ClaimType: "TRAVEL", OrganizationID: "org", Applicant: "applicant", ApprovedContentHash: hash, ItemID: "item", AllocationKey: allocationKey, ProjectID: "project", ExpenseNature: "PREPAYMENT", FeeType: "TRAVEL", OccurredDate: "2026-10-30", AmountMinor: 50, EvidenceDigest: hash}}}}}}
	svc.ReimbursementCosts = source
	in := recognitionTestInput(voucher, "claim-item", "COST", 50)
	in.SourceSystem = "reimbursement"
	in.SourceClaimID = "claim"
	in.SourceEffectID = "effect-1"
	in.SourceItemID = "item"
	in.SourceAllocationKey = allocationKey
	in.ProjectID = "project"
	in.CostCategory = "REIMBURSEMENT"
	before, e := svc.DashboardMetrics(ctx, tenant)
	if e != nil {
		t.Fatal(e)
	}
	var recognized int64
	var prepayment int64
	for _, item := range before.Rows {
		if item.Kind == "recognized_cost" {
			recognized += item.AmountMinor
		}
		if item.Kind == "prepayment_spend" {
			prepayment += item.AmountMinor
		}
	}
	fixtures["prepayment_cash_not_cost"] = before
	if recognized != 40 || prepayment != 50 {
		t.Fatalf("prepayment automatically became cost: cost=%d prepayment=%d", recognized, prepayment)
	}
	if _, e = svc.CreateRecognition(ctx, maker, "prepayment-no-basis", in); !errors.Is(e, ErrConflict) {
		t.Fatalf("prepayment without actual basis=%v", e)
	}
	in.PrepaymentBasis = "实际费用已发生并附完整摊销依据"
	row, e := svc.CreateRecognition(ctx, maker, "claim-create", in)
	if e != nil {
		t.Fatal(e)
	}
	row, e = svc.TransitionRecognition(ctx, maker, row.ID, "submit", "claim-submit", RecognitionDecision{ExpectedVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	row, e = svc.TransitionRecognition(ctx, reviewer, row.ID, "decision", "claim-confirm", RecognitionDecision{ExpectedVersion: 2, Decision: "CONFIRMED"})
	if e != nil {
		t.Fatal(e)
	}
	in.AmountMinor = 1
	if _, e = svc.CreateRecognition(ctx, maker, "over-limit", in); !errors.Is(e, ErrConflict) {
		t.Fatalf("stable limit=%v", e)
	}
	reverse := effect
	reverse.EffectID = "refund-1"
	reverse.Kind = "PAYMENT_FULLY_REVERSED"
	reverse.ReversesEffectID = effect.EffectID
	reverse.AmountMinor = "-50"
	reverse.Allocations = []CostAllocation{{ProjectID: "project", FeeType: "TRAVEL", AmountMinor: "-50"}}
	repay := effect
	repay.EffectID = "effect-2"
	repay.PaymentRecordID = "payment-2"
	source.snapshot.Claims[0].Effects = append(source.snapshot.Claims[0].Effects, reverse, repay)
	in.AmountMinor = 50
	in.SourceEffectID = "effect-2"
	if _, e = svc.CreateRecognition(ctx, maker, "blocked-repay", in); !errors.Is(e, ErrConflict) {
		t.Fatalf("refund and repay bypass=%v", e)
	}
	metrics, e := svc.DashboardMetrics(ctx, tenant)
	if e != nil {
		t.Fatal(e)
	}
	conflicts := 0
	for _, item := range metrics.Rows {
		if item.Kind == "recognition_conflict" {
			conflicts++
		}
	}
	fixtures["refund_repaid_still_conflict"] = metrics
	if conflicts != 1 {
		t.Fatalf("conflicts=%d", conflicts)
	}
	reversal := row.RecognitionInput
	reversal.OriginalID = row.ID
	reversal.Note = "原付款已全额冲正，独立复核冲回"
	reversal.RecognitionDate = "2026-11-02"
	reversal.AmountMinor = 51
	if _, e = svc.CreateRecognition(ctx, maker, "over-reverse", reversal); !errors.Is(e, ErrConflict) {
		t.Fatalf("over reverse=%v", e)
	}
	reversal.AmountMinor = 50
	undo, e := svc.CreateRecognition(ctx, maker, "reverse", reversal)
	if e != nil {
		t.Fatal(e)
	}
	undo, e = svc.TransitionRecognition(ctx, maker, undo.ID, "submit", "reverse-submit", RecognitionDecision{ExpectedVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	undo, e = svc.TransitionRecognition(ctx, reviewer, undo.ID, "decision", "reverse-confirm", RecognitionDecision{ExpectedVersion: 2, Decision: "CONFIRMED"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = svc.CreateRecognition(ctx, maker, "released-repay", in); e != nil {
		t.Fatalf("cleared difference did not release: %v", e)
	}
	resolved, e := svc.DashboardMetrics(ctx, tenant)
	if e != nil {
		t.Fatal(e)
	}
	fixtures["refund_reversal_cleared_repayment_draft"] = resolved
	source.snapshot.Claims[0].Status = "UNKNOWN"
	source.snapshot.Claims[0].UnknownReason = "历史审批快照缺失"
	source.snapshot.Claims[0].FrozenItems = nil
	unknown, e := svc.DashboardMetrics(ctx, tenant)
	if e != nil {
		t.Fatal(e)
	}
	fixtures["historical_payment_unknown"] = unknown
	original, e := svc.GetRecognition(ctx, maker, row.ID, "settlement.recognition.read")
	if e != nil || original.Version != 3 || original.Status != "CONFIRMED" || original.NetMinor != 50 {
		t.Fatalf("original mutated=%+v err=%v", original, e)
	}
}
