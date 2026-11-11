package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recognitionFixFixture 创建只具有新财务动作与ORG范围的主体和真实隔离文件证据。
// 参数：t为测试上下文，数据库仅通过当前授权私有DSN打开。
// 返回值：真实账本服务、分离的登记/复核主体及READY本地证据引用。
func recognitionFixFixture(t *testing.T) (*Service, Principal, Principal, string) {
	t.Helper()
	tenant := "recognition-fix-" + newID()
	svc := &Service{DB: dashboardFinanceMySQL(t), RecognitionDirectory: recognitionTestDirectory{}, ProjectReferences: recognitionTestProjects{}, RecognitionEnvironment: "local"}
	maker := recognitionTestPrincipal(tenant, "maker", "settlement_accountant")
	reviewer := recognitionTestPrincipal(tenant, "reviewer", "settlement_reviewer")
	reference := recognitionTestFileEvidence(t, svc, maker)
	return svc, maker, reviewer, reference
}

// recognitionTestFileEvidence 实际持久化上传绑定意图，保持文件网关为明确测试边界。
// 参数：t/svc为真实隔离账本，p为有权登记者。
// 返回值：READY本地file引用；上传或绑定失败直接终止测试。
func recognitionTestFileEvidence(t *testing.T, svc *Service, p Principal) string {
	t.Helper()
	evidence, e := svc.UploadRecognitionEvidence(context.Background(), p, "file-"+newID(), InvoiceArchiveIdentity{TenantID: p.TenantID, ApplicationID: "settlement-app", GatewayURL: "http://isolated-files"}, &recognitionEvidenceGateway{}, RecognitionEvidenceInput{OrganizationID: "org", OrganizationWitnessID: "witness", Name: "独立财务凭证.pdf", MediaType: "application/pdf", Content: []byte("%PDF-1.4\nreview regression evidence")})
	if e != nil || evidence.Status != "READY" {
		t.Fatalf("file evidence=%+v err=%v", evidence, e)
	}
	return "file:" + evidence.ID
}

// recognitionFixConfirm 通过真实提交及独立审核入口确认一份草稿。
// 参数：t/svc为测试账本，maker/reviewer为职责分离主体，row为真实草稿，key为独立命令前缀。
// 返回值：已独立确认记录；任何状态或授权错误终止测试。
func recognitionFixConfirm(t *testing.T, svc *Service, maker, reviewer Principal, row Recognition, key string) Recognition {
	t.Helper()
	ctx := context.Background()
	submitted, e := svc.TransitionRecognition(ctx, maker, row.ID, "submit", key+"-submit", RecognitionDecision{ExpectedVersion: row.Version})
	if e != nil {
		t.Fatal(e)
	}
	confirmed, e := svc.TransitionRecognition(ctx, reviewer, row.ID, "decision", key+"-confirm", RecognitionDecision{ExpectedVersion: submitted.Version, Decision: "CONFIRMED"})
	if e != nil {
		t.Fatal(e)
	}
	return confirmed
}

// recognitionFixCostSource 构造与真实源契约一致的安全整数付款与冻结费用。
// 参数：tenant为隔离租户，amount为付款整数分。
// 返回值：完整同租户local来源，分摊身份由规范项目/服务项重算。
func recognitionFixCostSource(tenant string, amount int64) *recognitionTestSource {
	raw, _ := json.Marshal([]string{"project", ""})
	allocation := sha256.Sum256(raw)
	hash := strings.Repeat("a", 64)
	effect := CostEffect{EffectID: "original-payment", PaymentRecordID: "payment", Kind: "PAYMENT_RECORDED", AmountMinor: strconv.FormatInt(amount, 10), OccurredAt: "2026-11-01T00:00:00Z", RecordedAt: "2026-11-01T00:00:00Z", ApprovedContentHash: hash, Allocations: []CostAllocation{{ProjectID: "project", FeeType: "TRAVEL", AmountMinor: strconv.FormatInt(amount, 10)}}}
	return &recognitionTestSource{snapshot: CostSnapshot{SchemaVersion: 1, SourceApplication: "reimbursement", SourceEnvironment: "local", TenantID: tenant, SnapshotAt: time.Now().UTC().Format(time.RFC3339Nano), Complete: true, Claims: []CostClaim{{ClaimID: "claim", ClaimNo: "QA", DocumentRevision: 3, ApprovalRound: 1, ProjectionRevision: 1, Currency: "CNY", Applicant: "applicant", OrganizationID: "org", Status: "KNOWN", Effects: []CostEffect{effect}, FrozenItems: []CostItem{{OrganizationID: "org", Applicant: "applicant", ApprovedContentHash: hash, ItemID: "item", AllocationKey: hex.EncodeToString(allocation[:]), ProjectID: "project", ExpenseNature: "ACTUAL", FeeType: "TRAVEL", OccurredDate: "2026-10-30", AmountMinor: amount, EvidenceDigest: hash}}}}}}
}

// TestRecognitionReviewR1ResidualTaxMySQL 验证净额耗尽后可独立处置残税并释放退款差异。
// 参数：t为当前私有隔离MySQL测试上下文。
// 返回值：无，仅税冲回、自审/超额防护或不可变原记录错误时失败。
func TestRecognitionReviewR1ResidualTaxMySQL(t *testing.T) {
	svc, maker, reviewer, evidence := recognitionFixFixture(t)
	ctx := context.Background()
	source := recognitionFixCostSource(maker.TenantID, 106)
	svc.ReimbursementCosts = source
	item := source.snapshot.Claims[0].FrozenItems[0]
	in := recognitionTestInput(evidence, "tax-source", "COST", 100)
	in.TaxMinor = 6
	in.SourceSystem = "reimbursement"
	in.SourceClaimID = "claim"
	in.SourceEffectID = "original-payment"
	in.SourceItemID = "item"
	in.SourceAllocationKey = item.AllocationKey
	in.ProjectID = "project"
	original, e := svc.CreateRecognition(ctx, maker, "original", in)
	if e != nil {
		t.Fatal(e)
	}
	original = recognitionFixConfirm(t, svc, maker, reviewer, original, "original")
	reverse := source.snapshot.Claims[0].Effects[0]
	reverse.EffectID = "refund"
	reverse.Kind = "PAYMENT_FULLY_REVERSED"
	reverse.ReversesEffectID = "original-payment"
	reverse.AmountMinor = "-106"
	reverse.Allocations = []CostAllocation{{ProjectID: "project", FeeType: "TRAVEL", AmountMinor: "-106"}}
	repay := source.snapshot.Claims[0].Effects[0]
	repay.EffectID = "new-payment"
	repay.PaymentRecordID = "new-payment"
	source.snapshot.Claims[0].Effects = append(source.snapshot.Claims[0].Effects, reverse, repay)
	netOnly := original.RecognitionInput
	netOnly.OriginalID = original.ID
	netOnly.TaxMinor = 0
	netRow, e := svc.CreateRecognition(ctx, maker, "net-only", netOnly)
	if e != nil {
		t.Fatal(e)
	}
	recognitionFixConfirm(t, svc, maker, reviewer, netRow, "net-only")
	remaining, e := svc.DashboardMetrics(ctx, maker.TenantID)
	if e != nil {
		t.Fatal(e)
	}
	recognitionFixMetricFixture(t, "residual_tax_pending", remaining)
	taxOnly := original.RecognitionInput
	taxOnly.OriginalID = original.ID
	taxOnly.AmountMinor = 0
	taxOnly.TaxMinor = 6
	taxOnly.TaxBasis = "EXCLUSIVE"
	taxRow, e := svc.CreateRecognition(ctx, maker, "residual-tax", taxOnly)
	if e != nil {
		t.Fatalf("F3-R1 residual tax cannot be registered: %v", e)
	}
	if taxRow.NetMinor != 0 || taxRow.GrossMinor != 6 {
		t.Fatalf("tax-only amounts=%+v", taxRow)
	}
	inclusive := taxOnly
	inclusive.TaxBasis = "INCLUSIVE"
	inclusive.AmountMinor = 6
	inclusiveRow, e := svc.CreateRecognition(ctx, maker, "residual-tax-inclusive", inclusive)
	if e != nil || inclusiveRow.NetMinor != 0 || inclusiveRow.GrossMinor != 6 {
		t.Fatalf("inclusive tax-only=%+v err=%v", inclusiveRow, e)
	}
	nonexistent := taxOnly
	nonexistent.OriginalID = newID()
	if _, e = svc.CreateRecognition(ctx, maker, "fake-original-tax", nonexistent); !errors.Is(e, ErrNotFound) {
		t.Fatalf("unproven original accepted: %v", e)
	}
	ordinary := taxOnly
	ordinary.OriginalID = ""
	ordinary.SourceSystem = "manual"
	ordinary.SourceClaimID = ""
	ordinary.SourceFactID = "ordinary-zero"
	if _, e = svc.CreateRecognition(ctx, maker, "ordinary-zero", ordinary); !errors.Is(e, ErrInvalid) {
		t.Fatalf("ordinary zero-net accepted: %v", e)
	}
	ordinaryInclusive := ordinary
	ordinaryInclusive.TaxBasis = "INCLUSIVE"
	ordinaryInclusive.AmountMinor = 6
	if _, e = svc.CreateRecognition(ctx, maker, "ordinary-inclusive-zero", ordinaryInclusive); !errors.Is(e, ErrInvalid) {
		t.Fatalf("ordinary inclusive zero-net accepted: %v", e)
	}
	zeroEffect := taxOnly
	zeroEffect.TaxMinor = 0
	if _, e = svc.CreateRecognition(ctx, maker, "empty-reversal", zeroEffect); !errors.Is(e, ErrInvalid) {
		t.Fatalf("zero-effect reversal accepted: %v", e)
	}
	submitted, e := svc.TransitionRecognition(ctx, maker, taxRow.ID, "submit", "tax-submit", RecognitionDecision{ExpectedVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	self := recognitionTestPrincipal(maker.TenantID, maker.UserID, "settlement_admin")
	if _, e = svc.TransitionRecognition(ctx, self, taxRow.ID, "decision", "tax-self", RecognitionDecision{ExpectedVersion: submitted.Version, Decision: "CONFIRMED"}); !errors.Is(e, ErrForbidden) {
		t.Fatalf("tax-only self review=%v", e)
	}
	if _, e = svc.TransitionRecognition(ctx, reviewer, taxRow.ID, "decision", "tax-confirm", RecognitionDecision{ExpectedVersion: submitted.Version, Decision: "CONFIRMED"}); e != nil {
		t.Fatal(e)
	}
	excess := taxOnly
	excess.TaxMinor = 1
	if _, e = svc.CreateRecognition(ctx, maker, "tax-over-limit", excess); !errors.Is(e, ErrConflict) {
		t.Fatalf("excess residual tax=%v", e)
	}
	in.SourceEffectID = "new-payment"
	if _, e = svc.CreateRecognition(ctx, maker, "released-after-tax", in); e != nil {
		t.Fatalf("residual tax left permanent freeze: %v", e)
	}
	cleared, e := svc.DashboardMetrics(ctx, maker.TenantID)
	if e != nil {
		t.Fatal(e)
	}
	for _, row := range cleared.Rows {
		if row.Kind == "recognition_conflict" {
			t.Fatal("residual tax difference still published after independent clearing")
		}
	}
	recognitionFixMetricFixture(t, "residual_tax_cleared", cleared)
	saved, e := svc.GetRecognition(ctx, maker, original.ID, "settlement.recognition.read")
	if e != nil || saved.NetMinor != 100 || saved.TaxMinor != 6 || saved.Version != 3 {
		t.Fatalf("original changed=%+v err=%v", saved, e)
	}
}

// TestRecognitionReviewR2SourceConservation 验证真实HTTP来源解码逐hash复验已知费用，不受整单UNKNOWN影响。
// 参数：t为当前来源fixture与本地HTTP测试上下文。
// 返回值：无，已知金额篡改或额外冻结分摊被接受时失败。
func TestRecognitionReviewR2SourceConservation(t *testing.T) {
	path := os.Getenv("SETTLEMENT_COST_SOURCE_FIXTURE_FILE")
	if path == "" {
		t.Skip("real isolated reimbursement fixture required")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var packet struct {
		Data CostSnapshot `json:"data"`
	}
	if json.Unmarshal(raw, &packet) != nil {
		t.Fatal("invalid fixture")
	}
	mixed := packet.Data
	claim := &mixed.Claims[0]
	claim.Status = "UNKNOWN"
	claim.UnknownReason = "旧付款缺批准快照"
	old := claim.Effects[0]
	old.EffectID = "unknown-payment"
	old.PaymentRecordID = "unknown-payment"
	old.Kind = "PAYMENT_RECORDED"
	old.ReversesEffectID = ""
	old.AmountMinor = "20"
	old.ApprovedContentHash = strings.Repeat("c", 64)
	old.Allocations = []CostAllocation{{ProjectID: "p", FeeType: "TRAVEL", AmountMinor: "20"}}
	refund := old
	refund.EffectID = "unknown-refund"
	refund.Kind = "PAYMENT_FULLY_REVERSED"
	refund.ReversesEffectID = old.EffectID
	refund.AmountMinor = "-20"
	refund.Allocations = []CostAllocation{{ProjectID: "p", FeeType: "TRAVEL", AmountMinor: "-20"}}
	claim.Effects = append(claim.Effects, old, refund)
	known := recognitionFixCostSource("known-fixture", 40).snapshot
	cases := []struct {
		name     string
		snapshot CostSnapshot
		bad      bool
	}{{"合法known101与unknown20", mixed, false}, {"UNKNOWN已知14改15", mixed, true}, {"KNOWN额外Q1", known, true}}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			bytes, _ := json.Marshal(test.snapshot)
			var snapshot CostSnapshot
			_ = json.Unmarshal(bytes, &snapshot)
			if test.name == "UNKNOWN已知14改15" {
				changed := false
				for index := range snapshot.Claims[0].FrozenItems {
					item := &snapshot.Claims[0].FrozenItems[index]
					if item.AmountMinor == 14 {
						item.AmountMinor = 15
						changed = true
						break
					}
				}
				if !changed {
					t.Fatal("fixture did not contain expected independent 14 allocation")
				}
			}
			if test.name == "KNOWN额外Q1" {
				extra := snapshot.Claims[0].FrozenItems[0]
				extra.ItemID = "extra-item"
				extra.ProjectID = "Q"
				extra.AmountMinor = 1
				raw, _ := json.Marshal([]string{extra.ProjectID, ""})
				digest := sha256.Sum256(raw)
				extra.AllocationKey = hex.EncodeToString(digest[:])
				snapshot.Claims[0].FrozenItems = append(snapshot.Claims[0].FrozenItems, extra)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/oauth2/token" {
					_, _ = w.Write([]byte(`{"access_token":"isolated-source-token","token_type":"Bearer","expires_in":900,"scope":"reimbursement.cost_candidate.read"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "OK", "data": snapshot})
			}))
			defer server.Close()
			source, e := NewReimbursementCosts(server.URL+"/cost", server.URL+"/oauth2/token", "isolated-client", "isolated-secret", snapshot.TenantID, "local")
			if e != nil {
				t.Fatal(e)
			}
			result, e := source.ReadCosts(context.Background(), snapshot.TenantID, "")
			if test.bad && !errors.Is(e, ErrDependency) {
				t.Fatalf("F3-R2 malformed known allocations accepted: claims=%d err=%v", len(result.Claims), e)
			}
			if !test.bad && (e != nil || !result.Complete) {
				t.Fatalf("valid mixed source rejected: %v", e)
			}
		})
	}
}

// recognitionFixVoucher 创建当前私有隔离库中的真实核销关联会计凭证。
// 参数：t/svc为隔离账本，tenant为本用例独立租户。
// 返回值：同租户CNY会计凭证ID，不授予调用者任何权限。
func recognitionFixVoucher(t *testing.T, svc *Service, tenant string) string {
	t.Helper()
	id, receipt, allocation, voucher := newID(), newID(), newID(), newID()
	dashboardFinanceSeed(t, svc.DB, tenant, id, "CNY", "RELEASED")
	_, e := svc.DB.Exec(`INSERT INTO settlement_receipt(id,tenant_id,receipt_no,customer_id,customer_name_snapshot,amount,unallocated_amount,currency,receipt_date,payment_method,bank_transaction_reference,source_type,status,idempotency_key,created_at,updated_at) VALUES(?,?,?,'customer','QA','100','60','CNY','2026-11-01','BANK',?,'MANUAL','AVAILABLE',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, receipt, tenant, receipt, receipt, receipt)
	if e != nil {
		t.Fatal(e)
	}
	_, e = svc.DB.Exec(`INSERT INTO settlement_receipt_allocation(id,tenant_id,allocation_no,receipt_id,receivable_id,allocated_amount,currency,status,match_mode,match_confidence,confirmed_by,confirmed_at,idempotency_key,created_at) VALUES(?,?,?,?,?,'40','CNY','CONFIRMED','MANUAL',100,'maker',UTC_TIMESTAMP(3),?,UTC_TIMESTAMP(3))`, allocation, tenant, allocation, receipt, id, allocation)
	if e != nil {
		t.Fatal(e)
	}
	_, e = svc.DB.Exec(`INSERT INTO settlement_accounting_voucher(id,tenant_id,writeoff_id,payload,status,created_at) VALUES(?,?,?,'{}','NOT_CONFIGURED',UTC_TIMESTAMP(3))`, voucher, tenant, allocation)
	if e != nil {
		t.Fatal(e)
	}
	return voucher
}

// TestRecognitionReviewR3VoucherACLMySQL 验证引用旧凭证必须沿用原动作及完整财务范围。
// 参数：t为真实私有隔离MySQL上下文。
// 返回值：无，缺旧凭证权限或原范围可创建、修改、提交或确认时失败。
func TestRecognitionReviewR3VoucherACLMySQL(t *testing.T) {
	svc, maker, reviewer, file := recognitionFixFixture(t)
	ctx := context.Background()
	voucher := recognitionFixVoucher(t, svc, maker.TenantID)
	in := recognitionTestInput(voucher, "old-voucher", "INCOME", 100)
	t.Run("只有新动作无旧凭证权限", func(t *testing.T) {
		if _, e := svc.CreateRecognition(ctx, maker, "no-voucher-permission", in); !errors.Is(e, ErrForbidden) {
			t.Errorf("F3-R3 old voucher was usable without old permission: %v", e)
		}
	})
	narrow := recognitionTestPrincipal(maker.TenantID, maker.UserID, "settlement_accountant")
	narrow.Permissions["settlement.voucher.manage"] = true
	t.Run("有旧权限但没有原完整范围", func(t *testing.T) {
		input := in
		input.SourceFactID = "narrow-voucher"
		if _, e := svc.CreateRecognition(ctx, narrow, "no-voucher-scope", input); !errors.Is(e, ErrForbidden) {
			t.Errorf("F3-R3 old voucher was usable without old scope: %v", e)
		}
	})
	t.Run("本组织独立文件仍可登记", func(t *testing.T) {
		input := recognitionTestInput(file, "file-remains-supported", "INCOME", 100)
		if _, e := svc.CreateRecognition(ctx, maker, "file-without-old-voucher", input); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("每次变更重验旧凭证ACL", func(t *testing.T) {
		authorized := recognitionTestPrincipal(maker.TenantID, maker.UserID, "settlement_accountant")
		authorized.Permissions["settlement.voucher.manage"] = true
		authorized.DataScopes = []DataScope{{RoleCode: "settlement_accountant", ScopeType: "TENANT"}}
		input := in
		input.SourceFactID = "authorized-voucher"
		row, e := svc.CreateRecognition(ctx, authorized, "authorized-voucher", input)
		if e != nil {
			t.Fatal(e)
		}
		edit := row.RecognitionInput
		edit.Note = "补充凭证说明"
		if _, e = svc.UpdateRecognition(ctx, maker, row.ID, "edit-without-voucher", RecognitionEdit{RecognitionInput: edit, ExpectedVersion: 1}); !errors.Is(e, ErrForbidden) {
			t.Errorf("old voucher edit bypass=%v", e)
		}
		if _, e = svc.TransitionRecognition(ctx, maker, row.ID, "submit", "submit-without-voucher", RecognitionDecision{ExpectedVersion: 1}); !errors.Is(e, ErrForbidden) {
			t.Errorf("old voucher submit bypass=%v", e)
		}
		row, e = svc.TransitionRecognition(ctx, authorized, row.ID, "submit", "submit-with-voucher", RecognitionDecision{ExpectedVersion: 1})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = svc.TransitionRecognition(ctx, reviewer, row.ID, "decision", "review-without-voucher", RecognitionDecision{ExpectedVersion: row.Version, Decision: "CONFIRMED"}); !errors.Is(e, ErrForbidden) {
			t.Errorf("old voucher review bypass=%v", e)
		}
		manager := recognitionTestPrincipal(maker.TenantID, "manager", "settlement_finance_manager")
		manager.Permissions["settlement.voucher.manage"] = true
		manager.DataScopes = []DataScope{{RoleCode: "settlement_finance_manager", ScopeType: "TENANT"}}
		if _, e = svc.TransitionRecognition(ctx, manager, row.ID, "decision", "review-with-voucher", RecognitionDecision{ExpectedVersion: row.Version, Decision: "CONFIRMED"}); e != nil {
			t.Fatal(e)
		}
	})
}

type recognitionReviewDirectory struct{}

// RecognitionPeople 提供显式分别属于A和B组织的人员目录边界。
// 参数：ctx为请求，id为精确人员，keyword为可选搜索词。
// 返回值：B人员仅属于other组织，其余依据人员属于org组织。
func (recognitionReviewDirectory) RecognitionPeople(ctx context.Context, id, keyword string) ([]RecognitionPerson, error) {
	if id == "outside-person" {
		return []RecognitionPerson{{ID: id, Name: "仅属于B的人员", Organizations: []RecognitionOrganization{{ID: "other", Name: "B组织"}}}}, nil
	}
	return recognitionTestDirectory{}.RecognitionPeople(ctx, id, keyword)
}

// TestRecognitionReviewR4IdentityVisibilityMySQL 验证直接提交人员ID不能绕过候选目录的ORG可见范围。
// 参数：t为真实私有隔离MySQL上下文，人员目录为明确测试边界。
// 返回值：无，范围外人员被登记、修改或复核接受时失败；显式跨ORG授权应保留。
func TestRecognitionReviewR4IdentityVisibilityMySQL(t *testing.T) {
	svc, maker, reviewer, file := recognitionFixFixture(t)
	svc.RecognitionDirectory = recognitionReviewDirectory{}
	ctx := context.Background()
	in := recognitionTestInput(file, "outside-person-cost", "COST", 40)
	in.IdentityID = "outside-person"
	if _, e := svc.CreateRecognition(ctx, maker, "outside-person-create", in); !errors.Is(e, ErrForbidden) {
		t.Errorf("F3-R4 direct out-of-scope identity accepted: %v", e)
	}
	base := recognitionTestInput(file, "identity-edit", "COST", 40)
	draft, e := svc.CreateRecognition(ctx, maker, "identity-edit-create", base)
	if e != nil {
		t.Fatal(e)
	}
	edit := draft.RecognitionInput
	edit.IdentityID = "outside-person"
	if _, e = svc.UpdateRecognition(ctx, maker, draft.ID, "outside-person-edit", RecognitionEdit{RecognitionInput: edit, ExpectedVersion: 1}); !errors.Is(e, ErrForbidden) {
		t.Errorf("out-of-scope identity edit accepted: %v", e)
	}
	cross := maker
	cross.DataScopes = append(append([]DataScope{}, maker.DataScopes...), DataScope{RoleCode: "settlement_accountant", ScopeType: "ORG", ScopeID: "other"})
	draft, e = svc.UpdateRecognition(ctx, cross, draft.ID, "explicit-cross-organization", RecognitionEdit{RecognitionInput: edit, ExpectedVersion: draft.Version})
	if e != nil || draft.IdentityName != "仅属于B的人员" {
		t.Fatalf("explicit visible cross-organization assignment failed: %+v %v", draft, e)
	}
	submitted, e := svc.TransitionRecognition(ctx, cross, draft.ID, "submit", "visible-identity-submit", RecognitionDecision{ExpectedVersion: draft.Version})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = svc.TransitionRecognition(ctx, reviewer, draft.ID, "decision", "outside-person-review", RecognitionDecision{ExpectedVersion: submitted.Version, Decision: "CONFIRMED"}); !errors.Is(e, ErrForbidden) {
		t.Errorf("out-of-scope identity review accepted: %v", e)
	}
	wideReviewer := reviewer
	wideReviewer.DataScopes = append(append([]DataScope{}, reviewer.DataScopes...), DataScope{RoleCode: "settlement_reviewer", ScopeType: "ORG", ScopeID: "other"})
	if _, e = svc.TransitionRecognition(ctx, wideReviewer, draft.ID, "decision", "visible-identity-confirm", RecognitionDecision{ExpectedVersion: submitted.Version, Decision: "CONFIRMED"}); e != nil {
		t.Fatal(e)
	}
}

// recognitionFixMetricFixture 保存当前真实隔离账本场景供主任务跨模块解码复验。
// 参数：t为上下文，name为本轮场景名，snapshot为实际DashboardMetrics成功输出。
// 返回值：无；未指定输出路径不写文件，写入失败终止测试且不输出凭据。
func recognitionFixMetricFixture(t *testing.T, name string, snapshot RecognitionMetrics) {
	t.Helper()
	path := os.Getenv("SETTLEMENT_METRIC_FIXTURE_PATH")
	if path == "" {
		return
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var bundle struct {
		Source    string                        `json:"source"`
		Scenarios map[string]RecognitionMetrics `json:"scenarios"`
	}
	if json.Unmarshal(raw, &bundle) != nil || bundle.Scenarios == nil {
		t.Fatal("existing metric fixture unavailable")
	}
	bundle.Scenarios[name] = snapshot
	raw, e = json.MarshalIndent(bundle, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
}
