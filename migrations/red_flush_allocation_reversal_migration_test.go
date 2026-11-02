package migrations

import (
	"strings"
	"testing"
)

// TestRedFlushReversalUniqueMigrationIsAdditiveAndFailClosed 验证反冲销幂等唯一键由新增迁移交付。
// 参数：t 测试上下文。
// 返回值：无。
func TestRedFlushReversalUniqueMigrationIsAdditiveAndFailClosed(t *testing.T) {
	body, err := Files.ReadFile("000023_red_flush_allocation_reversal_unique.sql")
	if err != nil {
		t.Fatalf("new red flush reversal migration is missing: %v", err)
	}
	text := string(body)
	for _, required := range []string{
		"ALTER TABLE settlement_invoice_allocation_reversal",
		"ADD UNIQUE KEY uq_settlement_allocation_reversal_allocation(tenant_id,original_allocation_id)",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("migration missing %s", required)
		}
	}
	// 财务事实不得由迁移清理：重复存量只能人工核对，迁移本身必须失败关闭。
	for _, forbidden := range []string{"DROP ", "DELETE ", "TRUNCATE "} {
		if strings.Contains(strings.ToUpper(text), forbidden) {
			t.Fatalf("migration must not destroy financial facts: %s", forbidden)
		}
	}
}
