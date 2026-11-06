package service

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// receiptIdempotencyBody 复刻 CreateReceipt 绑定到幂等键的请求内容形状。
// 参数：in 为已归一化的回款输入，normalizedAmount 为 amount() 归一化金额。
// 返回值：可 json.Marshal 的内容摘要载荷。
func receiptIdempotencyBody(in ReceiptInput, normalizedAmount string) any {
	return struct {
		CustomerID    string `json:"customer_id"`
		Amount        string `json:"amount"`
		Currency      string `json:"currency"`
		ReceiptDate   string `json:"receipt_date"`
		PaymentMethod string `json:"payment_method"`
		BankReference string `json:"bank_transaction_reference"`
	}{in.CustomerID, normalizedAmount, in.Currency, in.ReceiptDate, in.PaymentMethod, in.BankReference}
}

// reversalIdempotencyBody 复刻 ReverseAllocation 绑定到幂等键的请求内容形状。
// 参数：allocationID 为原核销，normalizedAmount 为归一化金额，in 为冲销原因。
// 返回值：可 json.Marshal 的内容摘要载荷。
func reversalIdempotencyBody(allocationID, normalizedAmount string, in ReversalInput) any {
	return struct {
		AllocationID string `json:"allocation_id"`
		Amount       string `json:"amount"`
		ReasonCode   string `json:"reason_code"`
		ReasonDetail string `json:"reason_detail"`
	}{allocationID, normalizedAmount, in.ReasonCode, in.ReasonDetail}
}

// redFlushIdempotencyBody 复刻 RequestInvoiceRedFlush 绑定到幂等键的请求内容形状。
// 参数：invoiceID 为原票，reasonCode/reasonDetail 为归一化原因。
// 返回值：可 json.Marshal 的内容摘要载荷。
func redFlushIdempotencyBody(invoiceID, reasonCode, reasonDetail string) any {
	return struct {
		InvoiceID    string `json:"invoice_id"`
		ReasonCode   string `json:"reason_code"`
		ReasonDetail string `json:"reason_detail"`
	}{invoiceID, reasonCode, reasonDetail}
}

// idempotencyHash 返回请求内容载荷的 SHA-256 摘要，模拟已落库的 request_hash。
// 参数：body 为内容摘要载荷。
// 返回值：32 字节摘要。
func idempotencyHash(t *testing.T, body any) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	hash := sha256.Sum256(raw)
	return hash[:]
}

// TestReceiptIdempotencyBindsRequestContent 验证回款同键同内容回放原结果，
// 同键改金额必须返回冲突而不是旧成功。
// 参数：t 为测试上下文。
// 返回值：无；回放丢失或异内容被当成首次成功时失败。
func TestReceiptIdempotencyBindsRequestContent(t *testing.T) {
	original := ReceiptInput{CustomerID: "customer-1", CustomerName: "客户", Amount: "100.00", Currency: "CNY", ReceiptDate: "2020-02-01", PaymentMethod: "TRANSFER", BankReference: "bank-1"}
	stored := idempotencyHash(t, receiptIdempotencyBody(original, "100.00"))
	replay := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{stored, "receipt-existing"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	id, err := (&Service{DB: replay}).CreateReceipt(context.Background(), Principal{TenantID: "tenant", UserID: "clerk"}, "receipt-key", original)
	if err != nil || id != "receipt-existing" {
		t.Fatalf("same content replay must return the original receipt: id=%q err=%v", id, err)
	}
	changed := original
	changed.Amount = "200.00"
	mismatch := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{stored, "receipt-existing"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	if _, err = (&Service{DB: mismatch}).CreateReceipt(context.Background(), Principal{TenantID: "tenant", UserID: "clerk"}, "receipt-key", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed amount under the same key must conflict: %v", err)
	}
}

// TestReversalIdempotencyBindsRequestContent 验证冲销同键同内容回放原结果，
// 同键改金额或原因必须返回冲突。
// 参数：t 为测试上下文。
// 返回值：无；异内容回放被当成成功时失败。
func TestReversalIdempotencyBindsRequestContent(t *testing.T) {
	original := ReversalInput{Amount: "10.00", ReasonCode: "MATCH_ERROR", ReasonDetail: "匹配错误"}
	stored := idempotencyHash(t, reversalIdempotencyBody("allocation-1", "10.00", original))
	replay := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{stored, "reversal-existing"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	id, err := (&Service{DB: replay}).ReverseAllocation(context.Background(), Principal{TenantID: "tenant", UserID: "manager"}, "reversal-key", "allocation-1", original)
	if err != nil || id != "reversal-existing" {
		t.Fatalf("same content replay must return the original reversal: id=%q err=%v", id, err)
	}
	changedAmount := original
	changedAmount.Amount = "20.00"
	changedReason := original
	changedReason.ReasonDetail = "改成其他原因"
	for name, in := range map[string]ReversalInput{"amount": changedAmount, "reason": changedReason} {
		t.Run(name, func(t *testing.T) {
			mismatch := scriptdb.Open(t,
				scriptdb.Step{Kind: "begin"},
				scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
				scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{stored, "reversal-existing"}}},
				scriptdb.Step{Kind: "rollback"},
			)
			if _, err := (&Service{DB: mismatch}).ReverseAllocation(context.Background(), Principal{TenantID: "tenant", UserID: "manager"}, "reversal-key", "allocation-1", in); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed %s under the same key must conflict: %v", name, err)
			}
		})
	}
}

// TestRedFlushIdempotencyBindsRequestContent 验证红冲申请同键同内容回放原结果，
// 同键改原因必须返回冲突。
// 参数：t 为测试上下文。
// 返回值：无；异内容回放被当成成功时失败。
func TestRedFlushIdempotencyBindsRequestContent(t *testing.T) {
	stored := idempotencyHash(t, redFlushIdempotencyBody("invoice-1", "INVOICE_ERROR", "税额错误"))
	replay := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{stored, "red-flush-existing"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	id, err := (&Service{DB: replay}).RequestInvoiceRedFlush(context.Background(), Principal{TenantID: "tenant", UserID: "requester"}, "red-key", "invoice-1", InvoiceRedFlushInput{ReasonCode: "INVOICE_ERROR", ReasonDetail: "税额错误"})
	if err != nil || id != "red-flush-existing" {
		t.Fatalf("same content replay must return the original request: id=%q err=%v", id, err)
	}
	mismatch := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record", Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		scriptdb.Step{Kind: "query", SQL: "SELECT request_hash,resource_id", Rows: [][]driver.Value{{stored, "red-flush-existing"}}},
		scriptdb.Step{Kind: "rollback"},
	)
	if _, err := (&Service{DB: mismatch}).RequestInvoiceRedFlush(context.Background(), Principal{TenantID: "tenant", UserID: "requester"}, "red-key", "invoice-1", InvoiceRedFlushInput{ReasonCode: "INVOICE_ERROR", ReasonDetail: "税率错误"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed reason under the same key must conflict: %v", err)
	}
}
