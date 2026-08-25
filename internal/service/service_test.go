package service

import "testing"

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

func TestDecimalGreaterUsesExactDecimalComparison(t *testing.T) {
	if !decimalGreater("10.01", "10.00") {
		t.Fatal("expected exact decimal comparison")
	}
	if decimalGreater("10.00", "10.00") {
		t.Fatal("equal values must not exceed")
	}
}
