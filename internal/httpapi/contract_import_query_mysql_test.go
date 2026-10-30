package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/service"
)

// TestImportFactsRealMySQL 验证工作台读取真实来源映射、分页及租户隔离。
// 参数：t为测试上下文，必须提供显式隔离MySQL测试DSN。
// 返回值：无；读取错误、客户字段缺失或跨租户泄漏即失败。
func TestImportFactsRealMySQL(t *testing.T) {
	db := roleSQLMySQL(t)
	s := &service.Service{DB: db, ContractImportEnabled: true}
	command := service.ContractImportCommand{EventID: roleSQLID(t), EventType: "contract.import_financial.v1", SchemaVersion: 1,
		Phase: "PREPARE", TenantID: "facts-tenant", OriginImportItemID: roleSQLID(t), SourceRevision: 1,
		PolicyDigest: strings.Repeat("a", 64), ReleaseVersion: 1, ImportMode: "CONTINUE_LIVE", OccurredAt: time.Now().UTC(),
		ExpectedFacts: []service.ExpectedImportFact{{SourceSystem: "legacy", FactType: "RECEIPT", ExternalFactID: "Original-A"}, {SourceSystem: "legacy", FactType: "RECEIPT", ExternalFactID: "Original-B"}},
		Contract:      json.RawMessage(`{"id":"facts-contract","no":"EXT-FACTS","version":1,"customer_id":"42","customer_name":"历史客户","owner_user_id":"owner","currency":"CNY","amount":"100.00","payment_terms":[{"installment_no":1,"due_date":"2020-01-01","amount":"100.00"}]}`)}
	receipt, err := s.PrepareContractImport(context.Background(), command.TenantID, command)
	if err != nil {
		t.Fatal(err)
	}
	p := service.Principal{TenantID: command.TenantID, UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	for _, key := range []string{"Original-A", "Original-B"} {
		_, err = s.CreateReceipt(context.Background(), p, key, service.ReceiptInput{ImportFact: &service.ImportFact{ContractSnapshotID: receipt.ContractSnapshotID, SourceSystem: "legacy", FactType: "RECEIPT", ExternalFactID: key}, CustomerID: "42", CustomerName: "历史客户", Amount: "10.00", Currency: "CNY", ReceiptDate: "2017-03-05", PaymentMethod: "TRANSFER", BankReference: key})
		if err != nil {
			t.Fatal(err)
		}
	}
	a := &API{service: s}
	request := httptest.NewRequest("GET", "/?limit=1&offset=1", nil)
	response := httptest.NewRecorder()
	a.importFacts(response, request, command.TenantID, receipt.ContractSnapshotID)
	if response.Code != 200 {
		t.Fatalf("HTTP=%d %s", response.Code, response.Body.String())
	}
	var body struct {
		Data struct {
			Total int                 `json:"total"`
			Items []map[string]string `json:"items"`
		}
	}
	if err = json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.Total != 2 || len(body.Data.Items) != 1 || body.Data.Items[0]["external_fact_id"] != "Original-B" || body.Data.Items[0]["target_business_id"] == "" {
		t.Fatalf("unexpected mappings: %+v", body.Data)
	}
	response = httptest.NewRecorder()
	a.importFacts(response, request, "another-tenant", receipt.ContractSnapshotID)
	if response.Code != 404 {
		t.Fatalf("cross-tenant HTTP=%d", response.Code)
	}
	status, err := a.importStatus(request, command.TenantID, receipt.ContractSnapshotID)
	if err != nil || status["policy_digest"] != command.PolicyDigest || status["import_mode"] != command.ImportMode || status["customer_id"] != "42" || status["source_contract_no"] != "EXT-FACTS" {
		t.Fatalf("status customer fields=%+v %v", status, err)
	}
}
