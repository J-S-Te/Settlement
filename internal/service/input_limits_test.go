package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSettlementTextLimits(t *testing.T) {
	service := &Service{}
	_, err := service.ReverseAllocation(context.Background(), Principal{}, "key", "allocation", ReversalInput{
		Amount: "1.00", ReasonCode: "OTHER", ReasonDetail: strings.Repeat("因", 501),
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("overlong reversal reason error = %v", err)
	}
	_, err = service.CreateDunningPolicy(context.Background(), Principal{}, DunningPolicyInput{
		Name: strings.Repeat("策", 256), AgingFromDays: 0, AgingToDays: 30, RepeatIntervalDays: 1,
		RecipientRule: "USER:1", Channel: "LOCAL", Priority: "NORMAL",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("overlong policy name error = %v", err)
	}
	_, err = service.CreateReceipt(context.Background(), Principal{}, "key", ReceiptInput{
		CustomerID: "customer", CustomerName: "客户", Amount: "1.00", Currency: "CNY",
		ReceiptDate: "2026-10-18", PaymentMethod: "TRANSFER", BankReference: strings.Repeat("A", 129),
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("overlong bank reference error = %v", err)
	}
	_, err = service.CreateInvoiceRequest(context.Background(), Principal{}, "key", InvoiceRequestInput{
		ContractSnapshotID: "contract",
		InvoiceType:        "SPECIAL",
		Items: []InvoiceItemInput{{
			ItemName: strings.Repeat("项", 256), TaxClassificationCode: "CODE", Quantity: "1",
			UnitPriceExclTax: "1", AmountExclTax: "1", TaxRate: "0.06", TaxAmount: "0.06", AmountInclTax: "1.06",
		}},
		Allocations: []InvoiceAllocationInput{{ReceivableID: "receivable", ReservedAmount: "1.06"}},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("overlong invoice item error = %v", err)
	}
}
