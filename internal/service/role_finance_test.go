package service

import (
	"errors"
	"testing"
)

// TestFinanceSettingsDefaults 验证五档账龄及空特殊阈值。
// 参数：t 测试上下文。
// 返回值：无。
func TestFinanceSettingsDefaults(t *testing.T) {
	s := defaultSettlementSettings()
	if len(s.AgingDays) != 4 || s.ReminderLeadDays != 3 || s.RepeatDays != 7 || s.EscalationDays != 30 || s.SpecialInvoiceThreshold != "" {
		t.Fatalf("defaults %+v", s)
	}
	s.AgingDays = []int{30, 30, 90, 180}
	if !errors.Is(validateSettlementSettings(s), ErrInvalid) {
		t.Fatal("duplicate aging boundary accepted")
	}
}

// TestSpecialInvoiceRule 验证金额阈值和特殊原因独立触发。
// 参数：t 测试上下文。
// 返回值：无。
func TestSpecialInvoiceRule(t *testing.T) {
	for _, c := range []struct {
		total, threshold, reason string
		want                     bool
	}{{"100", "", "", false}, {"100", "100", "", false}, {"101", "100", "", true}, {"1", "", "折扣", true}} {
		if specialInvoiceRequired(c.total, c.threshold, c.reason) != c.want {
			t.Fatalf("rule %+v", c)
		}
	}
}

// TestFinancePermissionBeforeDatabase 验证未授权主体在数据库前被拒绝。
// 参数：t 测试上下文。
// 返回值：无。
func TestFinancePermissionBeforeDatabase(t *testing.T) {
	s := &Service{}
	if _, err := s.GetSettlementSettings(t.Context(), Principal{}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := s.CreateAccountingVoucher(t.Context(), Principal{}, "x"); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}

// TestReceiptDocumentPermission 验证回单影像不可借用发票写权限。
// 参数：t 测试上下文。
// 返回值：无。
func TestReceiptDocumentPermission(t *testing.T) {
	_, _, err := (&Service{}).UploadReceiptDocument(t.Context(), Principal{Permissions: map[string]bool{"settlement.invoice.issue": true}}, InvoiceArchiveIdentity{}, nil, InvoiceArchiveInput{})
	if !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}

// TestSettingsFieldPolicyRejectsUnknown 验证只开放已实施字段策略。
// 参数：t 测试上下文。
// 返回值：无。
func TestSettingsFieldPolicyRejectsUnknown(t *testing.T) {
	in := defaultSettlementSettings()
	in.FieldPolicies = map[string]bool{"unknown": false}
	if !errors.Is(validateSettlementSettings(in), ErrInvalid) {
		t.Fatal("unknown policy accepted")
	}
}

// TestDifferenceRequiresIdempotency 验证余额暂挂不能接受没有幂等键的写入。
// 参数：t 测试上下文。
// 返回值：无。
func TestDifferenceRequiresIdempotency(t *testing.T) {
	_, err := (&Service{}).CreateReceiptDifference(t.Context(), Principal{Permissions: map[string]bool{"settlement.difference.manage": true}}, ReceiptDifferenceInput{ReceiptID: "receipt", Kind: "UNKNOWN", Amount: "10", Reason: "不明款"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
