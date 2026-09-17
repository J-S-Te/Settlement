package service

import (
	"context"
	"errors"
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
