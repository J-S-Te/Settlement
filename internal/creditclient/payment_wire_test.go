package creditclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const legacyCompletion = `{"event_id":"wire-completed","event_type":"customer.credit.payment.completed.v1","tenant_id":"trusted-tenant","receipt_id":"receipt-1","allocation_id":"allocation-1","payment_id":"allocation-1","customer_id":42,"due_date":"2026-08-31T00:00:00Z","paid_date":"2026-08-31T00:00:00Z","due_amount":"100.00","paid_amount":"100.00","contract_no":"WIRE-1","period_no":"1","source_system":"settlement"}`

type paymentWireCase struct {
	Legacy json.RawMessage `json:"legacy"`
	Body   json.RawMessage `json:"body"`
}

// TestPublishNormalizesPersistedPaymentWire 验证持久化旧报文经真实HTTP出站移除内部字段。
// 参数：t为测试上下文，可选环境变量指定跨仓报文字节文件。
// 返回值：字段丢失、内部字段泄漏或HTTP投递失败时测试失败。
func TestPublishNormalizesPersistedPaymentWire(t *testing.T) {
	var rows []paymentWireCase
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		rows = append(rows, paymentWireCase{Body: body})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := &Client{endpoint: server.URL, client: server.Client()}
	reversed := strings.ReplaceAll(legacyCompletion, `"wire-completed"`, `"wire-reversed"`)
	reversed = strings.ReplaceAll(reversed, `"customer.credit.payment.completed.v1"`, `"customer.credit.payment.reversed.v1","reversal_of_payment_id":"allocation-1"`)
	reversed = strings.ReplaceAll(reversed, `"payment_id":"allocation-1"`, `"payment_id":"allocation-1-reversed"`)
	reversed = strings.ReplaceAll(reversed, `"paid_amount":"100.00"`, `"paid_amount":"0.00"`)
	for _, legacy := range []string{legacyCompletion, reversed} {
		if err := client.Publish(context.Background(), "trusted-tenant", "wire", []byte(legacy)); err != nil {
			t.Fatal(err)
		}
		row := &rows[len(rows)-1]
		row.Legacy = json.RawMessage(legacy)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row.Body, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"tenant_id", "receipt_id", "allocation_id"} {
			if _, exists := fields[key]; exists {
				t.Errorf("internal field leaked: %s", key)
			}
		}
		var original map[string]json.RawMessage
		_ = json.Unmarshal(row.Legacy, &original)
		for key, value := range original {
			if key == "tenant_id" || key == "receipt_id" || key == "allocation_id" {
				continue
			}
			if string(fields[key]) != string(value) {
				t.Errorf("field %s changed: %s -> %s", key, value, fields[key])
			}
		}
	}
	if path := os.Getenv("CRM_CREDIT_WIRE_FILE"); path != "" {
		raw, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPublishRejectsMalformedPaymentWire 验证规范化不会吞掉未知字段或畸形内容。
// 参数：t为测试上下文。
// 返回值：非法报文到达HTTP端点或被接受时失败。
func TestPublishRejectsMalformedPaymentWire(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts++; w.WriteHeader(204) }))
	defer server.Close()
	client := &Client{endpoint: server.URL, client: server.Client()}
	for _, payload := range []string{
		`{"event_id":"a","event_id":"b"}`, `{"Event_ID":"a"}`, `{"unknown":true}`,
		`{"customer_id":"42"}`, `{"tenant_id":{}}`, `{"paid_date":"bad-time"}`,
		`{"due_amount":100}`, `{} {}`, `null`, `[]`, `{"payment_id":null}`,
		`{"tenant_id":"wrong-tenant"}`,
	} {
		if err := client.Publish(context.Background(), "tenant", "event", []byte(payload)); err == nil {
			t.Errorf("accepted invalid wire: %s", payload)
		}
	}
	if posts != 0 {
		t.Errorf("invalid posts=%d", posts)
	}
}
