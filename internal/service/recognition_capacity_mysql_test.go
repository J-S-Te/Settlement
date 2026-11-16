package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestRecognitionUnrelatedCapacityMySQL 验证超过万条无关草稿不阻塞手工确认、来源确认或原确认冲回。
// 参数：t为显式隔离MySQL测试上下文，未配置私有DSN时跳过。
// 返回值：无，范围、税额、幂等、独立复核与并发余额守恒错误时失败。
func TestRecognitionUnrelatedCapacityMySQL(t *testing.T) {
	tenant := "capacity-" + newID()
	svc := &Service{DB: financeOperationMySQL(t), RecognitionDirectory: recognitionTestDirectory{}, ProjectReferences: recognitionTestProjects{}, RecognitionEnvironment: "local"}
	maker := recognitionTestPrincipal(tenant, "maker", "settlement_accountant")
	reviewer := recognitionTestPrincipal(tenant, "reviewer", "settlement_reviewer")
	evidence := recognitionTestFileEvidence(t, svc, maker)
	ctx := context.Background()
	originalInput := recognitionTestInput(evidence, "original", "INCOME", 100)
	originalInput.TaxMinor = 6
	original, err := svc.CreateRecognition(ctx, maker, "original", originalInput)
	if err != nil {
		t.Fatal(err)
	}
	original = recognitionFixConfirm(t, svc, maker, reviewer, original, "original")
	tx, err := svc.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO settlement_recognition(id,tenant_id,organization_id,identity_id,status,source_claim_id,original_id,payload,created_at,updated_at) VALUES(?,?,?,'','DRAFT','unrelated','',?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`)
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	for index := 0; index < 10001; index++ {
		row := original
		row.ID = newID()
		row.Status = "DRAFT"
		row.SourceClaimID = "unrelated"
		raw, _ := json.Marshal(row)
		if _, err = statement.ExecContext(ctx, row.ID, maker.TenantID, "org", raw); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	manual, err := svc.CreateRecognition(ctx, maker, "manual", recognitionTestInput(evidence, "manual", "INCOME", 1))
	if err != nil {
		t.Fatal(err)
	}
	manual = recognitionFixConfirm(t, svc, maker, reviewer, manual, "manual")
	if manual.Status != "CONFIRMED" {
		t.Fatal("manual confirmation blocked by unrelated drafts")
	}
	reversal := original.RecognitionInput
	reversal.OriginalID = original.ID
	reversal.AmountMinor = 60
	reversal.TaxMinor = 7
	if _, err = svc.CreateRecognition(ctx, maker, "excess-tax", reversal); !errors.Is(err, ErrConflict) {
		t.Fatalf("excess reversal tax accepted: %v", err)
	}
	reversal.TaxMinor = 4
	drafts := make([]Recognition, 2)
	for index := range drafts {
		drafts[index], err = svc.CreateRecognition(ctx, maker, fmt.Sprintf("reverse-%d", index), reversal)
		if err != nil {
			t.Fatal(err)
		}
		drafts[index], err = svc.TransitionRecognition(ctx, maker, drafts[index].ID, "submit", fmt.Sprintf("reverse-submit-%d", index), RecognitionDecision{ExpectedVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for index, row := range drafts {
		wg.Add(1)
		go func(index int, row Recognition) {
			defer wg.Done()
			_, err := svc.TransitionRecognition(ctx, reviewer, row.ID, "decision", fmt.Sprintf("reverse-confirm-%d", index), RecognitionDecision{ExpectedVersion: row.Version, Decision: "CONFIRMED"})
			results <- err
		}(index, row)
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("concurrent reversal success=%d conflict=%d", success, conflicts)
	}
	source := recognitionFixCostSource(maker.TenantID, 100)
	svc.ReimbursementCosts = source
	input := recognitionTestInput(evidence, "cost", "COST", 60)
	item := source.snapshot.Claims[0].FrozenItems[0]
	input.SourceSystem = "reimbursement"
	input.SourceClaimID = "claim"
	input.SourceEffectID = "original-payment"
	input.SourceItemID = "item"
	input.SourceAllocationKey = item.AllocationKey
	input.ProjectID = "project"
	cost, err := svc.CreateRecognition(ctx, maker, "cost", input)
	if err != nil {
		t.Fatal(err)
	}
	input.Note = "万条无关草稿后的合法编辑"
	cost, err = svc.UpdateRecognition(ctx, maker, cost.ID, "cost-edit", RecognitionEdit{RecognitionInput: input, ExpectedVersion: cost.Version})
	if err != nil {
		t.Fatalf("source draft edit blocked by unrelated rows: %v", err)
	}
	submitted, err := svc.TransitionRecognition(ctx, maker, cost.ID, "submit", "cost-before-return", RecognitionDecision{ExpectedVersion: cost.Version})
	if err != nil {
		t.Fatal(err)
	}
	returned, err := svc.TransitionRecognition(ctx, reviewer, cost.ID, "decision", "cost-return", RecognitionDecision{ExpectedVersion: submitted.Version, Decision: "RETURNED", Note: "独立退回补充依据"})
	if err != nil {
		t.Fatal(err)
	}
	cost, err = svc.UpdateRecognition(ctx, maker, returned.ID, "cost-returned-edit", RecognitionEdit{RecognitionInput: input, ExpectedVersion: returned.Version})
	if err != nil {
		t.Fatalf("returned source edit blocked by unrelated rows: %v", err)
	}
	recognitionFixConfirm(t, svc, maker, reviewer, cost, "cost")
	input.AmountMinor = 41
	if _, err = svc.CreateRecognition(ctx, maker, "cost-over", input); !errors.Is(err, ErrConflict) {
		t.Fatalf("source cap bypassed: %v", err)
	}
	if _, err = svc.RecognitionCandidates(ctx, maker, "claim"); err != nil {
		t.Fatalf("precise candidates blocked: %v", err)
	}
}
