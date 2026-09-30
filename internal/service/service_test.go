package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAmountRejectsFloatLikeOrOverPrecisionValues(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "12.345", "abc"} {
		if _, err := amount(value); err == nil {
			t.Fatalf("amount(%q) should fail", value)
		}
	}
	got, err := amount("123.4")
	if err != nil || got != "123.40" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSuggestedAllocationRequiresConfidence(t *testing.T) {
	service := &Service{}
	_, err := service.ConfirmAllocation(context.Background(), Principal{}, "key", AllocationInput{
		Amount: "10.00", MatchMode: "SUGGESTED", MatchConfidence: 0,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected invalid confidence, got %v", err)
	}
}

func TestAllocationRejectsUnknownMatchModeBeforeDatabaseAccess(t *testing.T) {
	service := &Service{}
	_, err := service.ConfirmAllocation(context.Background(), Principal{}, "key", AllocationInput{
		Amount: "10.00", MatchMode: "AUTOMATIC", MatchConfidence: 90,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected invalid match mode, got %v", err)
	}
}

func TestDecimalGreaterUsesExactDecimalComparison(t *testing.T) {
	if !decimalGreater("10.01", "10.00") {
		t.Fatal("expected exact decimal comparison")
	}
	if decimalGreater("10.00", "10.00") {
		t.Fatal("equal values must not exceed")
	}
}

func TestInvoiceIssuanceModeFailsSafeToManual(t *testing.T) {
	if got := (&Service{}).invoiceIssuanceMode(); got != "manual" {
		t.Fatalf("default issuance mode = %q, want manual", got)
	}
	if got := (&Service{InvoiceIssuanceMode: "tax_adapter"}).invoiceIssuanceMode(); got != "tax_adapter" {
		t.Fatalf("configured issuance mode = %q, want tax_adapter", got)
	}
	if got := (&Service{InvoiceIssuanceMode: "unexpected"}).invoiceIssuanceMode(); got != "manual" {
		t.Fatalf("unknown issuance mode = %q, want fail-safe manual", got)
	}
}

// AUD-2026-001：报文体租户与验签可信租户不一致必须在触库前被拒绝，
// 持合法机器令牌的调用方不能声明任意租户写入财务数据。
func TestIngestContractRejectsCrossTenant(t *testing.T) {
	service := &Service{}
	event := ContractEvent{EventID: "E-1", TenantID: "tenant-b"}
	_, err := service.IngestContract(context.Background(), "contract_management", "tenant-a", event)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "verified machine tenant") {
		t.Fatalf("IngestContract cross-tenant err = %v, want ErrInvalid tenant mismatch", err)
	}
}

// AUD-2026-001：可信租户缺失时同样失败关闭，绝不采信报文租户。
func TestIngestContractRejectsUnresolvedTrustedTenant(t *testing.T) {
	service := &Service{}
	event := ContractEvent{EventID: "E-1", TenantID: "tenant-a"}
	_, err := service.IngestContract(context.Background(), "contract_management", "", event)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "verified machine tenant") {
		t.Fatalf("IngestContract empty trusted tenant err = %v, want ErrInvalid tenant mismatch", err)
	}
}

// AUD-2026-001 正向基线：租户一致时通过租户门禁进入后续校验
// （此处用无效 event_type 证明请求越过租户检查且未触库）。
func TestIngestContractSameTenantPassesTenantGate(t *testing.T) {
	service := &Service{}
	event := ContractEvent{EventID: "E-1", TenantID: "tenant-a", EventType: "unsupported.v1"}
	_, err := service.IngestContract(context.Background(), "contract_management", "tenant-a", event)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "unsupported event_type") {
		t.Fatalf("IngestContract same-tenant err = %v, want past tenant gate at event_type validation", err)
	}
	if strings.Contains(err.Error(), "verified machine tenant") {
		t.Fatalf("matching tenant must not be rejected by tenant gate: %v", err)
	}
}
