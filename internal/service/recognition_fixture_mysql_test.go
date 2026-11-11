package service

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestRecognitionMixedSourceFixtureMySQL 复验报销真实隔离源输出在结算侧的费用性质拆分及金额守恒。
// 参数：t为上下文，来源文件为报销真实MySQL生成，账本仅使用授权私有DSN。
// 返回值：无，逐项唯一、尾差、预付分列或未知历史污染已知金额时失败。
func TestRecognitionMixedSourceFixtureMySQL(t *testing.T) {
	path := os.Getenv("SETTLEMENT_COST_SOURCE_FIXTURE_FILE")
	if path == "" {
		t.Skip("real isolated reimbursement fixture required")
	}
	raw, e := os.ReadFile(path)
	if e != nil || len(raw) > 8<<20 {
		t.Fatal("source fixture unavailable")
	}
	var envelope struct {
		Code string       `json:"code"`
		Data CostSnapshot `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code != "OK" {
		t.Fatal("invalid source fixture")
	}
	source := &recognitionTestSource{snapshot: envelope.Data}
	if e = validateCostSnapshot(source.snapshot, source.snapshot.TenantID, "local", ""); e != nil {
		t.Fatal(e)
	}
	svc := &Service{DB: dashboardFinanceMySQL(t), RecognitionEnvironment: "local", ReimbursementCosts: source}
	snapshot, e := svc.DashboardMetrics(context.Background(), source.snapshot.TenantID)
	if e != nil {
		t.Fatal(e)
	}
	seen := map[string]bool{}
	var positive, negative, actualPositive, prepaymentPositive int64
	for _, row := range snapshot.Rows {
		if seen[row.RecordID] {
			t.Fatal("duplicate effect/item/allocation row")
		}
		seen[row.RecordID] = true
		if row.AmountMinor > 0 {
			positive += row.AmountMinor
			if row.Kind == "reimbursement_spend" {
				actualPositive += row.AmountMinor
			}
			if row.Kind == "prepayment_spend" {
				prepaymentPositive += row.AmountMinor
			}
		} else {
			negative += row.AmountMinor
		}
	}
	if len(snapshot.Rows) != 6 || positive != 101 || negative != -101 || actualPositive != 41 || prepaymentPositive != 60 {
		t.Fatalf("mixed allocation rows=%d positive=%d negative=%d actual=%d prepayment=%d", len(snapshot.Rows), positive, negative, actualPositive, prepaymentPositive)
	}
	output := map[string]RecognitionMetrics{"mixed_real_reimbursement_effects": snapshot}
	// 添加缺旧批准快照的真实历史形状；已知费用仍须保留，未知不重复进入分类金额。
	claim := &source.snapshot.Claims[0]
	claim.Status = "UNKNOWN"
	claim.UnknownReason = "旧付款缺批准快照"
	old := claim.Effects[0]
	old.EffectID = "unknown-old-payment"
	old.PaymentRecordID = "unknown-old-payment"
	old.Kind = "PAYMENT_RECORDED"
	old.ReversesEffectID = ""
	old.AmountMinor = "20"
	old.ApprovedContentHash = strings.Repeat("c", 64)
	old.Allocations = []CostAllocation{{ProjectID: "p", FeeType: "TRAVEL", AmountMinor: "20"}}
	refund := old
	refund.EffectID = "unknown-old-refund"
	refund.Kind = "PAYMENT_FULLY_REVERSED"
	refund.ReversesEffectID = old.EffectID
	refund.AmountMinor = "-20"
	refund.Allocations = []CostAllocation{{ProjectID: "p", FeeType: "TRAVEL", AmountMinor: "-20"}}
	claim.Effects = append(claim.Effects, old, refund)
	snapshot, e = svc.DashboardMetrics(context.Background(), source.snapshot.TenantID)
	if e != nil {
		t.Fatal(e)
	}
	known, unknown := 0, 0
	var knownPositive, unknownPositive int64
	for _, row := range snapshot.Rows {
		if row.Kind == "reimbursement_unknown" {
			unknown++
			if row.AmountMinor > 0 {
				unknownPositive += row.AmountMinor
			}
		} else {
			known++
			if row.AmountMinor > 0 {
				knownPositive += row.AmountMinor
			}
		}
	}
	if known != 6 || unknown != 2 || knownPositive != 101 || unknownPositive != 20 {
		t.Fatalf("partial unknown polluted known: known=%d unknown=%d money=%d/%d", known, unknown, knownPositive, unknownPositive)
	}
	output["mixed_known_and_unknown_history"] = snapshot
	if fixture := os.Getenv("SETTLEMENT_METRIC_FIXTURE_PATH"); fixture != "" {
		var bundle struct {
			Source    string                        `json:"source"`
			Scenarios map[string]RecognitionMetrics `json:"scenarios"`
		}
		existing, e := os.ReadFile(fixture)
		if e != nil || json.Unmarshal(existing, &bundle) != nil {
			t.Fatal("base metric fixture unavailable")
		}
		for name, snapshot := range output {
			bundle.Scenarios[name] = snapshot
		}
		encoded, e := json.MarshalIndent(bundle, "", "  ")
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(fixture, encoded, 0600); e != nil {
			t.Fatal(e)
		}
	}
}
