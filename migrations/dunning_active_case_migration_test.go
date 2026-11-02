package migrations

import (
	"strings"
	"testing"
)

// TestDunningActiveCaseUniqueMigrationIsAdditiveAndFailClosed 验证唯一 ACTIVE
// 案件约束由新增迁移交付：生成列只对 ACTIVE 行写入业务键，历史案件不被清理，
// 存量重复只通过预检暴露并让迁移失败关闭。
// 参数：t 测试上下文。
// 返回值：无。
func TestDunningActiveCaseUniqueMigrationIsAdditiveAndFailClosed(t *testing.T) {
	body, err := Files.ReadFile("000023_dunning_active_case_unique.sql")
	if err != nil {
		t.Fatalf("new dunning active case migration is missing: %v", err)
	}
	text := string(body)
	for _, required := range []string{
		"ALTER TABLE settlement_dunning_case",
		"ADD COLUMN active_receivable_id CHAR(32)",
		"ADD COLUMN active_plan_id CHAR(32)",
		"GENERATED ALWAYS AS (IF(status='ACTIVE',receivable_id,NULL)) VIRTUAL",
		"GENERATED ALWAYS AS (IF(status='ACTIVE',plan_id,NULL)) VIRTUAL",
		"ADD UNIQUE KEY uq_settlement_dunning_active_receivable (tenant_id,active_receivable_id)",
		"ADD UNIQUE KEY uq_settlement_dunning_active_plan (tenant_id,active_plan_id)",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("migration missing %s", required)
		}
	}
	// 预检 SQL 必须随迁移交付，且按应收和计划两个维度只读暴露历史重复。
	for _, required := range []string{
		"WHERE status='ACTIVE' AND receivable_id IS NOT NULL",
		"GROUP BY tenant_id,receivable_id HAVING COUNT(*)>1",
		"WHERE status='ACTIVE' AND plan_id IS NOT NULL",
		"GROUP BY tenant_id,plan_id HAVING COUNT(*)>1",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("migration missing read-only precheck %s", required)
		}
	}
	// 历史重复的处理由业务决定：迁移不得自动删除、合并、关闭或改写案件事实。
	upper := strings.ToUpper(text)
	for _, forbidden := range []string{"DELETE ", "TRUNCATE ", "UPDATE ", "DROP ", "SET DC.STATUS", "SET STATUS"} {
		if strings.Contains(upper, forbidden) {
			t.Fatalf("migration must not rewrite dunning case facts: %s", forbidden)
		}
	}
}
