package httpapi

import (
	"database/sql/driver"
	"testing"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestFinancialBasisProjectionPreservesContractColumn 验证真实扫描与JSON装饰不会把本地依据编号冒充合同。
// 参数：t 为测试上下文。
// 返回值：无；覆盖合同列、丢失本地来源或版本时失败。
func TestFinancialBasisProjectionPreservesContractColumn(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "projection", Rows: [][]driver.Value{{"LOCAL_FINANCIAL", "local-id", "FB-1", int64(2), "settlement_local"}}})
	var basis financialBasisProjection
	if err := db.QueryRow("projection").Scan(basis.targets()...); err != nil {
		t.Fatal(err)
	}
	item := basis.decorate(map[string]any{"contract_no": "", "amount": "100.00"})
	if item["contract_no"] != "" || item["basis_type"] != "LOCAL_FINANCIAL" || item["local_document_no"] != "FB-1" || item["local_document_version"] != 2 || item["customer_namespace"] != "settlement_local" {
		t.Fatalf("item=%+v", item)
	}
}
