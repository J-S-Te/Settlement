package service

import (
	"testing"
	"time"
)

// TestRecognitionExplicitScope 验证新权限不能继承legacy内部全范围例外。
// 参数：t为测试上下文。
// 返回值：无，缺少角色或范围被放行时失败。
func TestRecognitionExplicitScope(t *testing.T) {
	p := Principal{TenantID: "t", UserID: "maker", Permissions: map[string]bool{"settlement.recognition.record": true}}
	if recognitionAllowed(p, "settlement.recognition.record", "org") {
		t.Fatal("legacy unscoped principal accepted")
	}
	p.Roles = []string{"settlement_accountant"}
	p.CatalogVersion = "v"
	p.DataScopes = []DataScope{{RoleCode: "settlement_accountant", ScopeType: "ORG", ScopeID: "org"}}
	if !recognitionAllowed(p, "settlement.recognition.record", "org") || recognitionAllowed(p, "settlement.recognition.record", "other") {
		t.Fatal("organization scope not enforced")
	}
	p.Roles = []string{"settlement_system_admin"}
	p.DataScopes = []DataScope{{RoleCode: "settlement_system_admin", ScopeType: "TENANT"}}
	if recognitionAllowed(p, "settlement.recognition.record", "org") {
		t.Fatal("configuration admin acquired financial capability")
	}
}

// TestRecognitionTaxAndBusinessDates 验证税基、整数金额及业务发生日与确认日独立保留。
// 参数：t为测试上下文。
// 返回值：无，非法税基日期或金额被接受时失败。
func TestRecognitionTaxAndBusinessDates(t *testing.T) {
	in := RecognitionInput{Kind: "INCOME", OrganizationID: "org", OrganizationWitnessID: "person", Currency: "CNY", AmountMinor: 100, TaxMinor: 6, TaxBasis: "EXCLUSIVE", OccurredDate: "2026-10-30", RecognitionDate: "2026-11-01", EvidenceReference: "voucher", Note: "依据", SourceSystem: "manual", SourceEnvironment: "local", SourceFactID: "fact"}
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	if err := validateRecognition(in, now); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []RecognitionInput{func() RecognitionInput { x := in; x.TaxBasis = ""; return x }(), func() RecognitionInput { x := in; x.TaxMinor = -1; return x }(), func() RecognitionInput { x := in; x.AmountMinor = 9007199254740992; return x }(), func() RecognitionInput { x := in; x.OccurredDate = "2026-12-01"; return x }()} {
		if validateRecognition(bad, now) == nil {
			t.Fatal("accepted invalid recognition")
		}
	}
}
