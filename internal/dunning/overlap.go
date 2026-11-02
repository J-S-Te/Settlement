package dunning

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// caseQuerier 是重叠检查所需的最小查询能力，事务内外均可使用。
type caseQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// activeCaseConflict 用途：在同一应收或回款计划上查找已存在的 ACTIVE 催收案件，
// 排除当前策略版本自身的案件。存在时调用方必须复用或放弃创建，
// 不得为同一催收义务再建第二个 ACTIVE 案件。
// 参数：ctx为上下文，q为事务或连接，tenant为租户，receivableID/planID为催收对象，
// 二者至少一个非空；planID为空时由应收关联的出款计划补足。policyID/policyVersion
// 为本次准备创建案件的策略版本。
// 返回值：冲突案件标识；空字符串表示可创建。查询失败原样返回。
func activeCaseConflict(ctx context.Context, q caseQuerier, tenant, receivableID, planID, policyID string, policyVersion int) (string, error) {
	receivableID = strings.TrimSpace(receivableID)
	planID = strings.TrimSpace(planID)
	if receivableID == "" && planID == "" {
		return "", nil
	}
	conditions := []string{}
	args := []any{tenant}
	if planID != "" {
		conditions = append(conditions, "(plan_id IS NOT NULL AND plan_id=?)")
		args = append(args, planID)
	}
	if receivableID != "" {
		conditions = append(conditions, "(receivable_id IS NOT NULL AND receivable_id=?)")
		args = append(args, receivableID)
		if planID == "" {
			// 扫描器只持有应收标识；应收关联的计划同样属于同一催收义务。
			conditions = append(conditions, "(plan_id IS NOT NULL AND plan_id=(SELECT rp.receivable_plan_id FROM settlement_receivable rp WHERE rp.tenant_id=? AND rp.id=? AND rp.receivable_plan_id IS NOT NULL))")
			args = append(args, tenant, receivableID)
		}
	}
	args = append(args, policyID, policyVersion, planID)
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM settlement_dunning_case
WHERE tenant_id=? AND status='ACTIVE' AND (`+strings.Join(conditions, " OR ")+`)
  AND NOT (dunning_policy_id=? AND policy_version=? AND COALESCE(plan_id,'')=?)
LIMIT 1`, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}
