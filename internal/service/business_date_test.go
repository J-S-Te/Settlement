package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/businessclock"
)

// TestValidateBusinessDateOnRejectsFutureBeijingDay 验证业务日期只允许北京
// 业务日当天及以前，格式错误与未来日期都返回 ErrInvalid。
// 参数：t 为测试上下文。
// 返回值：无；未来日期被放行或今天/过去被拒绝时失败。
func TestValidateBusinessDateOnRejectsFutureBeijingDay(t *testing.T) {
	today := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, value string
		wantErr     bool
	}{
		{"today", "2026-01-02", false},
		{"past", "2025-12-31", false},
		{"future", "2026-01-03", true},
		{"bad format", "2026-1-2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateBusinessDateOn(tc.value, "receipt_date", today)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("value=%q err=%v, want ErrInvalid", tc.value, err)
				}
				return
			}
			if err != nil || got != tc.value {
				t.Fatalf("value=%q got=%q err=%v", tc.value, got, err)
			}
		})
	}
}

// TestBusinessDateUsesBeijingDayNotUTCDate 验证北京日边界：UTC 仍是前一日、
// 北京已是次日时，按北京业务日判定为“今天”而不是未来。
// 参数：t 为测试上下文。
// 返回值：无；退回 UTC 日期口径时失败。
func TestBusinessDateUsesBeijingDayNotUTCDate(t *testing.T) {
	// 2025-12-31 16:30Z = 北京 2026-01-01 00:30；UTC 日期仍是 12-31。
	instant := time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC)
	beijingDay := businessclock.DateString(instant)
	if beijingDay != "2026-01-01" {
		t.Fatalf("beijing day=%q, want 2026-01-01", beijingDay)
	}
	today := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := validateBusinessDateOn(beijingDay, "receipt_date", today); err != nil {
		t.Fatalf("beijing today must be accepted: %v", err)
	}
	if _, err := validateBusinessDateOn("2026-01-02", "receipt_date", today); !errors.Is(err, ErrInvalid) {
		t.Fatalf("next beijing day must be rejected: %v", err)
	}
}

// TestCreateReceiptRejectsFutureBusinessDate 验证回款日在写库前拒绝未来北京业务日。
// 参数：t 为测试上下文。
// 返回值：无；未来日期进入事务或未被拒绝时失败。
func TestCreateReceiptRejectsFutureBusinessDate(t *testing.T) {
	tomorrow := businessclock.DateString(businessclock.Today().AddDate(0, 0, 1))
	_, err := (&Service{}).CreateReceipt(context.Background(), Principal{}, "key", ReceiptInput{
		CustomerID: "customer", CustomerName: "客户", Amount: "1.00", Currency: "CNY",
		ReceiptDate: tomorrow, PaymentMethod: "TRANSFER", BankReference: "bank",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("future receipt_date must be rejected before the transaction: %v", err)
	}
}

// TestRefundBusinessDayComparisonUsesBeijingDay 验证退款完成时间与回款日按北京
// 业务日比较：北京次日 00:30（UTC 仍是前一日）不算早于回款日；北京前一日 23:59 才拒绝。
// 参数：t 为测试上下文。
// 返回值：无；按 UTC 时间戳比较导致错位时失败。
func TestRefundBusinessDayComparisonUsesBeijingDay(t *testing.T) {
	receiptDay := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	// 2026-01-01 16:30Z = 北京 2026-01-02 00:30，与回款日同日。
	if err := validateRefundReceiptBusinessDayOn(time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC), receiptDay); err != nil {
		t.Fatalf("beijing same day must pass: %v", err)
	}
	// 2026-01-01 15:59Z = 北京 2026-01-01 23:59，早于回款日。
	if err := validateRefundReceiptBusinessDayOn(time.Date(2026, 1, 1, 15, 59, 0, 0, time.UTC), receiptDay); !errors.Is(err, ErrInvalid) {
		t.Fatalf("beijing previous day must be rejected: %v", err)
	}
	// 同一北京业务日的后段时刻仍通过。
	if err := validateRefundReceiptBusinessDayOn(time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC), receiptDay); err != nil {
		t.Fatalf("later same beijing day must pass: %v", err)
	}
}
