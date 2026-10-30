package outbox

import (
	"context"
	"encoding/json"
	"fmt"
)

// filterImportNotifications 在实际投递前核对暂存状态及生成动作时的放行版本。
// 参数：ctx为投递上下文，events为已认领事件。
// 返回值：可投递事件；过期动作持久取消，数据库及报文错误保持失败。
func (s *Store) filterImportNotifications(ctx context.Context, events []Event) ([]Event, error) {
	result := make([]Event, 0, len(events))
	for _, event := range events {
		if event.Destination != "PLATFORM_NOTIFICATION" || (event.EventType != "SETTLEMENT_OWNER_REMINDER" && event.EventType != "SETTLEMENT_RECEIVABLE_OVERDUE") {
			result = append(result, event)
			continue
		}
		var payload struct {
			ReferenceType  string `json:"reference_type"`
			ReferenceID    string `json:"reference_id"`
			ReleaseVersion uint64 `json:"import_release_version"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, err
		}
		table := "settlement_receivable"
		if payload.ReferenceType == "RECEIVABLE_PLAN" {
			table = "settlement_receivable_plan"
		} else if payload.ReferenceType != "RECEIVABLE" {
			return nil, fmt.Errorf("invalid dunning notification reference")
		}
		var state string
		var version uint64
		err := s.DB.QueryRowContext(ctx, `SELECT cs.migration_state,COALESCE(ci.release_version,0) FROM `+table+` r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id LEFT JOIN settlement_contract_import ci ON ci.contract_snapshot_id=cs.id AND ci.tenant_id=cs.tenant_id WHERE r.tenant_id=? AND r.id=?`, event.TenantID, payload.ReferenceID).Scan(&state, &version)
		if err != nil {
			return nil, err
		}
		if state == "RELEASED" && version == payload.ReleaseVersion {
			result = append(result, event)
			continue
		}
		_, err = s.DB.ExecContext(ctx, `UPDATE settlement_outbox_event SET status='CANCELLED',last_error_code='IMPORT_HOLD_OR_STALE_RELEASE',last_error_summary='历史暂存或放行版本变化，旧动作禁止发送',locked_by=NULL,locked_until=NULL,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND status='PROCESSING' AND locked_by=?`, event.ID, s.WorkerID)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
