package migrations

import (
	"strings"
	"testing"
)

// TestLocalFinancialBaselineMigrationKeepsSourceIdentity 验证026增加独立本地命名空间且不伪造合同。
// 参数：t为测试上下文，只读取内嵌SQL，不连接数据库。
// 返回值：无；本地依据、可空合同身份或租户约束缺失时失败。
func TestLocalFinancialBaselineMigrationKeepsSourceIdentity(t *testing.T) {
	body, err := Files.ReadFile("000026_local_financial_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{"CREATE TABLE settlement_financial_customer", "PRIMARY KEY (tenant_id,id)",
		"MODIFY source_contract_id VARCHAR(128) NULL", "basis_type='LOCAL_FINANCIAL'", "source_application='settlement_local'",
		"source_contract_id IS NULL", "source_contract_no=''", "local_document_no IS NOT NULL",
		"customer_namespace='settlement_local'", "FOREIGN KEY (tenant_id,local_customer_id)"} {
		if !strings.Contains(text, required) {
			t.Fatalf("026 missing %s", required)
		}
	}
	for _, forbidden := range []string{"UPDATE settlement_contract_snapshot", "DROP TABLE", "FLOAT", "DOUBLE"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("026 rewrites facts or weakens amounts: %s", forbidden)
		}
	}
}
