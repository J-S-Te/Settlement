package service

import (
	"errors"
	"testing"
)

func TestAmountRejectsFractionalAndExponentNotation(t *testing.T) {
	for _, value := range []string{"0.125", "1/3", "1/8", "1e3", "1.234", "+1.00", ".50", "1.", "1,00"} {
		if _, err := amount(value); err == nil {
			t.Fatalf("amount(%q) should be rejected instead of silently rounded", value)
		}
	}
}

func TestDecimalRejectsFractionalAndExponentNotation(t *testing.T) {
	for _, value := range []string{"1/3", "1.1234567", "1e3", "abc", "-1"} {
		if _, err := decimal(value, false); err == nil {
			t.Fatalf("decimal(%q) should be rejected", value)
		}
	}
	for _, value := range []string{"0", "0.123456", "3", "100.00"} {
		if _, err := decimal(value, false); err != nil {
			t.Fatalf("decimal(%q) rejected: %v", value, err)
		}
	}
}

func TestInvoiceLineMustMatchQuantityPriceAndTax(t *testing.T) {
	_, _, _, _, err := normalizeInvoiceItems([]InvoiceItemInput{{
		ItemName: "mismatch", Quantity: "3", UnitPriceExclTax: "20",
		AmountExclTax: "100", TaxRate: "0.13", TaxAmount: "1", AmountInclTax: "101",
	}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched invoice line err=%v, want invalid input", err)
	}
}

func TestInvoiceLineRoundsOnlyOncePerMoneyComponent(t *testing.T) {
	normalized, excl, tax, incl, err := normalizeInvoiceItems([]InvoiceItemInput{{
		ItemName: "rounding", Quantity: "3", UnitPriceExclTax: "20",
		AmountExclTax: "60", TaxRate: "0.13", TaxAmount: "7.80", AmountInclTax: "67.80",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if normalized[0].Quantity != "3.000000" || excl.FloatString(2) != "60.00" || tax.FloatString(2) != "7.80" || incl.FloatString(2) != "67.80" {
		t.Fatalf("normalized=%+v totals=%s/%s/%s", normalized, excl.FloatString(2), tax.FloatString(2), incl.FloatString(2))
	}
}

func TestCreateInvoiceRequestRejectsMismatchedLineBeforeDatabase(t *testing.T) {
	service := &Service{}
	_, err := service.CreateInvoiceRequest(t.Context(), Principal{}, "mismatch", InvoiceRequestInput{
		Items: []InvoiceItemInput{{
			ItemName: "mismatch", Quantity: "3", UnitPriceExclTax: "20",
			AmountExclTax: "100", TaxRate: "0.13", TaxAmount: "1", AmountInclTax: "101",
		}},
		Allocations: []InvoiceAllocationInput{{ReceivableID: "receivable-1", ReservedAmount: "101"}},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched invoice request err=%v, want invalid input before database access", err)
	}
}
