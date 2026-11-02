package httpapi

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// revisionEventBody 构造机器入站财务修订事件报文。
// 参数：eventID 为事件标识，version 为合同版本，amount 为合同金额，terms 为付款计划。
// 返回值：按生产字段序列化的事件 JSON。
func revisionEventBody(t *testing.T, eventID string, version int, amount string, terms []service.PaymentTerm) []byte {
	t.Helper()
	event := service.ContractEvent{EventID: eventID, EventType: "contract.financial_effective.v1", TenantID: "tenant-a", OccurredAt: time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)}
	event.Contract.ID = "contract-1"
	event.Contract.No = "HT-1"
	event.Contract.Version = version
	event.Contract.CustomerID = "customer-1"
	event.Contract.CustomerName = "隔离客户"
	event.Contract.Currency = "CNY"
	event.Contract.Amount = amount
	event.Contract.PaymentTerms = terms
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// revisionIngestRequest 提交机器入站事件并返回响应记录。
// 参数：handler 为结算入口，body 为事件报文。
// 返回值：真实 HTTP 响应。
func revisionIngestRequest(handler http.Handler, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/settlement/events/contracts", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer integration-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestContractRevisionHoldReturnsAccepted 验证含已实现事实的修订以 202 接收并明示
// 人工复核，而不是 409 让合同侧空转重试。
// 参数：t 为测试上下文。
// 返回值：无；状态码或响应字段不符时失败。
func TestContractRevisionHoldReturnsAccepted(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_stream"},
		scriptdb.Step{Kind: "query", SQL: "SELECT current_version FROM settlement_contract_stream", Rows: [][]driver.Value{{int64(1)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_inbox_event"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id,contract_amount,migration_state FROM settlement_contract_snapshot", Rows: [][]driver.Value{{"snapshot-old", "100.00", "RELEASED"}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_revision_hold"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_inbox_event"},
		scriptdb.Step{Kind: "commit"},
	)
	handler := New(db, config.Config{DevelopmentAuth: true, IntegrationEnabled: true, IntegrationBearerToken: "integration-token", OIDCTenantID: "tenant-a"}, slog.Default(), nil, nil, nil)
	recorder := revisionIngestRequest(handler, revisionEventBody(t, "EVT-8", 2, "100.00", []service.PaymentTerm{{InstallmentNo: 1, DueDate: "2099-01-01", Amount: "90.00"}}))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("修订接收状态=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"accepted":true`) || !strings.Contains(body, `"revision_hold":true`) || !strings.Contains(body, `"duplicate":false`) {
		t.Fatalf("修订接收响应=%s", body)
	}
}

// TestContractRevisionVersionRegressionReturnsConflict 验证版本回退仍按冲突拒绝，
// 不会被当作修订自动应用。
// 参数：t 为测试上下文。
// 返回值：无；回退未返回 409 时失败。
func TestContractRevisionVersionRegressionReturnsConflict(t *testing.T) {
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT id FROM settlement_inbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_stream"},
		scriptdb.Step{Kind: "query", SQL: "SELECT current_version FROM settlement_contract_stream", Rows: [][]driver.Value{{int64(2)}}},
		scriptdb.Step{Kind: "rollback"},
	)
	handler := New(db, config.Config{DevelopmentAuth: true, IntegrationEnabled: true, IntegrationBearerToken: "integration-token", OIDCTenantID: "tenant-a"}, slog.Default(), nil, nil, nil)
	recorder := revisionIngestRequest(handler, revisionEventBody(t, "EVT-9", 1, "100.00", []service.PaymentTerm{{InstallmentNo: 1, DueDate: "2099-01-01", Amount: "100.00"}}))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("版本回退状态=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
