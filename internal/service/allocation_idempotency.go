package service

import (
	"context"
	"database/sql"
)

// claimAllocationIdempotency 将普通核销原键绑定操作人及规范业务内容。
// 参数：ctx/tx为已授权事务，p为主体，key/id为原键与候选ID，value为规范金额，in为核销内容。
// 返回值：原核销标识及回放标记；同键异内容或存储失败返回错误。
func claimAllocationIdempotency(ctx context.Context, tx *sql.Tx, p Principal,
	key, id, value string, in AllocationInput,
) (string, bool, error) {
	request := struct {
		ActorID         string `json:"actor_id"`
		ReceiptID       string `json:"receipt_id"`
		ReceivableID    string `json:"receivable_id"`
		Amount          string `json:"amount"`
		MatchMode       string `json:"match_mode"`
		MatchConfidence int    `json:"match_confidence"`
	}{p.UserID, in.ReceiptID, in.ReceivableID, value, in.MatchMode, in.MatchConfidence}
	return claimIdempotency(ctx, tx, p.TenantID, "CONFIRM_RECEIPT_ALLOCATION", key, id, request)
}
