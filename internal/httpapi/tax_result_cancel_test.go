package httpapi

import (
	"database/sql/driver"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// lateIssueCallbackBody 构造作废后迟到税控回执的机器报文。
// 参数：eventID 为事件标识，sequence 为回执序号。
// 返回值：可直接提交给税控结果入口的 JSON 报文。
func lateIssueCallbackBody(eventID string, sequence int64) string {
	body := map[string]any{
		"event_id": eventID, "event_type": "tax.invoice.issued.v1",
		"tenant_id": "tenant-a", "provider_code": "tax-provider",
		"occurred_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"result_sequence": sequence, "attempt_id": "attempt-1",
		"external_request_id": "external-request-1",
		"invoice": map[string]any{
			"external_invoice_id": "external-invoice-1", "invoice_code": "CODE-1", "invoice_no": "NO-1",
			"invoice_type": "VAT_NORMAL", "currency": "CNY",
			"amount_excl_tax": "100.00", "tax_amount": "13.00", "amount_incl_tax": "113.00",
			"issue_date": "2026-10-30", "seller_profile": map[string]any{"name": "隔离销方"},
			"items": []map[string]any{{
				"item_name": "服务", "quantity": "1", "unit_price_excl_tax": "100.00", "amount_excl_tax": "100.00",
				"tax_rate": "0.13", "tax_amount": "13.00", "amount_incl_tax": "113.00",
			}},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(payload)
}

// TestTaxResultAfterCancelReturnsAccepted 验证作废后的迟到"已开票"回执返回 2xx
// 并保留结果行，而不是 409 回滚把 inbox 与冲突事实一起丢掉；不重复释放额度。
// 参数：t 为测试上下文。
// 返回值：无；非 202、缺少 PROVIDER_CONFLICT 结果或出现额度写入时失败
// （scriptdb 对未脚本化的额度 SQL 直接判失败）。
func TestTaxResultAfterCancelReturnsAccepted(t *testing.T) {
	var auditArgs, inboxArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_tax_result_inbox"},
		scriptdb.Step{Kind: "query", SQL: "SELECT ia.invoice_request_id,ia.status,ir.status", Rows: [][]driver.Value{{
			"request-1", "ABANDONED", "CANCELLED", "external-request-1", "tax-provider", int64(0),
			"VAT_NORMAL", "CNY", "100.00", "13.00", "113.00", []byte(`{}`),
		}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_invoice_issue_attempt SET result_sequence=?"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: &auditArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_tax_result_inbox SET processing_status=?", Args: &inboxArgs},
		scriptdb.Step{Kind: "commit"},
	)
	handler := New(db, config.Config{DevelopmentAuth: true, TaxResultIngestEnabled: true, TaxResultBearerToken: "tax-secret", OIDCTenantID: "tenant-a", PublicOrigin: "https://settlement.test"}, slog.Default(), nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/tax-results", strings.NewReader(lateIssueCallbackBody("tax-event-1", 3)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer tax-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("作废后迟到回执必须 2xx，实际=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"processing_status":"PROVIDER_CONFLICT"`) {
		t.Fatalf("响应未明示冲突处理状态: %s", response.Body.String())
	}
	if len(inboxArgs) == 0 || inboxArgs[0].Value != "PROVIDER_CONFLICT" {
		t.Fatalf("结果行未标记冲突: %+v", inboxArgs)
	}
	if len(auditArgs) == 0 || auditArgs[4].Value != "SETTLEMENT_TAX_PROVIDER_CONFLICT" {
		t.Fatalf("缺少税控冲突审计: %+v", auditArgs)
	}
}
