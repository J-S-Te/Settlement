package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"github.com/j-s-te/settlement/internal/metricstream"
	"io"
	"strconv"
	"strings"
	"time"
)

// StreamDashboardMetrics 在有界RR游标中输出全部已登记确认与现金事实。
// 参数：ctx 为请求，tenant 为验签租户，output 为协议输出。
// 返回值：来源、读取、输出或提交失败返回错误；只有成功提交后才输出end。
func (s *Service) StreamDashboardMetrics(ctx context.Context, tenant string, output io.Writer) error {
	if tenant == "" || s.DB == nil {
		return ErrDependency
	}
	meta := RecognitionMetrics{SchemaVersion: 1, TenantID: tenant, SourceCode: "settlement", SnapshotAt: time.Now().UTC().Format(time.RFC3339Nano), Complete: true, Basis: "本系统已登记且财务独立复核的不含税净确认；冲回按本次确认月列负数；现金/预付独立列示；旧历史不猜填", Rows: []RecognitionMetricRow{}}
	var source CostSnapshot
	if s.ReimbursementCosts != nil {
		var err error
		source, err = s.ReimbursementCosts.ReadCosts(ctx, tenant, "")
		if err != nil {
			return ErrDependency
		}
		if err = validateCostSnapshot(source, tenant, s.RecognitionEnvironment, ""); err != nil {
			return err
		}
	} else {
		meta.Basis += "；报销现金来源未配置，未声明已接入"
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	meta.SnapshotAt = time.Now().UTC().Format(time.RFC3339Nano)
	stream, err := metricstream.New(output, meta)
	if err != nil {
		return err
	}
	if err = streamRecognitionLedger(ctx, tx, tenant, s.RecognitionEnvironment, source, stream, &meta); err != nil {
		return err
	}
	if err = streamRecognitionCash(tenant, source, stream, &meta); err != nil {
		return err
	}
	if err = stream.Flush(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return stream.End(meta.SourceUpdatedAt, meta.CollectionStartedAt)
}

// streamRecognitionLedger 按主键批读精简确认字段，并批量核对对应原记录的冲回余额。
// 参数：ctx/tx/tenant 为一致性快照，environment/source 为可信现金来源，stream/meta 接收输出与时间边界。
// 返回值：读取、源租户不符或输出失败返回错误，不使用全账本上限。
func streamRecognitionLedger(ctx context.Context, tx *sql.Tx, tenant, environment string, source CostSnapshot, stream *metricstream.Writer, meta *RecognitionMetrics) error {
	reversed := map[string]map[string]bool{}
	for _, claim := range source.Claims {
		effects := map[string]bool{}
		for _, effect := range claim.Effects {
			if effect.Kind == "PAYMENT_FULLY_REVERSED" {
				effects[effect.ReversesEffectID] = true
			}
		}
		reversed[claim.ClaimID] = effects
	}
	cursor := ""
	for {
		records, err := metricRecognitionBatch(ctx, tx, tenant, cursor)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		refunds, err := metricRecognitionRefunds(ctx, tx, tenant, records)
		if err != nil {
			return err
		}
		for _, record := range records {
			cursor = record.ID
			if meta.CollectionStartedAt == "" || record.CreatedAt.Before(parseMetricTime(meta.CollectionStartedAt)) {
				meta.CollectionStartedAt = record.CreatedAt.UTC().Format(time.RFC3339Nano)
			}
			if meta.SourceUpdatedAt == "" || record.UpdatedAt.After(parseMetricTime(meta.SourceUpdatedAt)) {
				meta.SourceUpdatedAt = record.UpdatedAt.UTC().Format(time.RFC3339Nano)
			}
			if record.Status != "CONFIRMED" && record.Status != "SUBMITTED" {
				continue
			}
			if len(record.RecognitionDate) != 10 {
				return ErrDependency
			}
			row := recognitionMetric(record)
			if record.Status == "SUBMITTED" {
				row.Kind = "recognition_pending"
				row.Status = "PENDING"
				row.AmountMinor = 0
				row.TaxMinor = 0
			} else if record.Kind == "INCOME" {
				row.Kind = "recognized_income"
			} else {
				row.Kind = "recognized_cost"
			}
			if err := stream.Add(row); err != nil {
				return err
			}
			refund := refunds[record.ID]
			if record.Status == "CONFIRMED" && record.SourceSystem == "reimbursement" && record.SourceEnvironment == environment && record.OriginalID == "" && reversed[record.SourceClaimID][record.SourceEffectID] && (record.NetMinor > refund[0] || record.GrossMinor > refund[1]) {
				row.Kind = "recognition_conflict"
				row.RecordID = "refund:" + record.ID
				row.Status = "CONFLICT"
				row.AmountMinor = 0
				row.TaxMinor = 0
				if err := stream.Add(row); err != nil {
					return err
				}
			}
		}
	}
}

// metricRecognitionBatch 只读取指标与退款核对所需字段，不加载凭证、说明或全部账本。
// 参数：ctx/tx 为事务，tenant 为可信租户，cursor 为上批最后主键。
// 返回值：至多256条精简确认；存储租户不符、读取或解码失败返回错误。
func metricRecognitionBatch(ctx context.Context, tx *sql.Tx, tenant, cursor string) ([]Recognition, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, JSON_OBJECT(
 'id',JSON_EXTRACT(payload,
 '$.id'),
 'tenant_id',JSON_EXTRACT(payload,
 '$.tenant_id'),
 'kind',JSON_EXTRACT(payload,
 '$.kind'),
 'cost_category',JSON_EXTRACT(payload,
 '$.cost_category'),
 'organization_id',JSON_EXTRACT(payload,
 '$.organization_id'),
 'identity_id',JSON_EXTRACT(payload,
 '$.identity_id'),
 'project_id',JSON_EXTRACT(payload,
 '$.project_id'),
 'organization_name',JSON_EXTRACT(payload,
 '$.organization_name'),
 'identity_name',JSON_EXTRACT(payload,
 '$.identity_name'),
 'project_name',JSON_EXTRACT(payload,
 '$.project_name'),
 'recognition_date',JSON_EXTRACT(payload,
 '$.recognition_date'),
 'currency',JSON_EXTRACT(payload,
 '$.currency'),
 'net_minor',JSON_EXTRACT(payload,
 '$.net_minor'),
 'gross_minor',JSON_EXTRACT(payload,
 '$.gross_minor'),
 'tax_minor',JSON_EXTRACT(payload,
 '$.tax_minor'),
 'status',JSON_EXTRACT(payload,
 '$.status'),
 'original_id',JSON_EXTRACT(payload,
 '$.original_id'),
 'source_system',JSON_EXTRACT(payload,
 '$.source_system'),
 'source_environment',JSON_EXTRACT(payload,
 '$.source_environment'),
 'source_claim_id',JSON_EXTRACT(payload,
 '$.source_claim_id'),
 'source_effect_id',JSON_EXTRACT(payload,
 '$.source_effect_id'),
 'created_at',JSON_EXTRACT(payload,
 '$.created_at'),
 'updated_at',JSON_EXTRACT(payload,
 '$.updated_at'))
 FROM settlement_recognition
 WHERE tenant_id=? AND id>? ORDER BY id LIMIT 256`, tenant, cursor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Recognition{}
	for rows.Next() {
		var raw []byte
		var id string
		var row Recognition
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if json.Unmarshal(raw, &row) != nil || row.TenantID != tenant || row.ID != id || row.ID <= cursor {
			return nil, ErrDependency
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// metricRecognitionRefunds 批量汇总一批原确认的已确认冲回，避免按行查询或加载全账本。
// 参数：ctx/tx 为同一快照，tenant 为租户，records 为当前有界批次。
// 返回值：原记录对应净额和含税冲回额；超范围或读取失败返回错误。
func metricRecognitionRefunds(ctx context.Context, tx *sql.Tx, tenant string, records []Recognition) (map[string][2]int64, error) {
	out := map[string][2]int64{}
	args := []any{tenant}
	marks := []string{}
	for _, row := range records {
		if row.OriginalID == "" && row.Status == "CONFIRMED" && row.SourceSystem == "reimbursement" {
			args = append(args, row.ID)
			marks = append(marks, "?")
		}
	}
	if len(marks) == 0 {
		return out, nil
	}
	query := `SELECT original_id,
 SUM(CAST(JSON_UNQUOTE(JSON_EXTRACT(payload,'$.net_minor')) AS SIGNED)),
 SUM(CAST(JSON_UNQUOTE(JSON_EXTRACT(payload,'$.gross_minor')) AS SIGNED))
 FROM settlement_recognition
 WHERE tenant_id=? AND status='CONFIRMED' AND original_id IN (` + strings.Join(marks, ",") + `) GROUP BY original_id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var net, gross int64
		if err := rows.Scan(&id, &net, &gross); err != nil {
			return nil, err
		}
		if net < 0 || gross < 0 || net > RecognitionMaximum || gross > RecognitionMaximum {
			return nil, ErrDependency
		}
		out[id] = [2]int64{net, gross}
	}
	return out, rows.Err()
}

// streamRecognitionCash 沿用原现金和预付事实身份与未知语义逐行输出。
// 参数：tenant/source 为可信完整来源，stream/meta 接收有界输出与更新时间。
// 返回值：输出失败时错误；不改写金额、历史或预付性质。
func streamRecognitionCash(tenant string, source CostSnapshot, stream *metricstream.Writer, meta *RecognitionMetrics) error {
	for _, claim := range source.Claims {
		for _, effect := range claim.Effects {
			if recorded := parseMetricTime(effect.RecordedAt); meta.SourceUpdatedAt == "" || recorded.After(parseMetricTime(meta.SourceUpdatedAt)) {
				meta.SourceUpdatedAt = recorded.UTC().Format(time.RFC3339Nano)
			}
			items := []CostItem{}
			for _, item := range claim.FrozenItems {
				if item.ApprovedContentHash == effect.ApprovedContentHash {
					items = append(items, item)
				}
			}
			if len(items) == 0 {
				value, _ := strconv.ParseInt(effect.AmountMinor, 10, 64)
				reason := claim.UnknownReason
				if reason == "" {
					reason = "缺少该付款对应的完整冻结费用及预付性质"
				}
				if err := stream.Add(RecognitionMetricRow{Kind: "reimbursement_unknown", RecordID: claim.ClaimID + ":" + effect.EffectID, Period: effect.OccurredAt[:7], Status: "UNKNOWN", Currency: claim.Currency, AmountMinor: value, UnknownReason: reason}); err != nil {
					return err
				}
				continue
			}
			sign := int64(1)
			if effect.Kind == "PAYMENT_FULLY_REVERSED" {
				sign = -1
			}
			for _, item := range items {
				kind := "reimbursement_spend"
				if item.ExpenseNature == "PREPAYMENT" {
					kind = "prepayment_spend"
				}
				identity, _ := json.Marshal([]string{source.SourceApplication, source.SourceEnvironment, tenant, claim.ClaimID, effect.EffectID, item.ItemID, item.AllocationKey})
				digest := sha256.Sum256(identity)
				if err := stream.Add(RecognitionMetricRow{Kind: kind, RecordID: hex.EncodeToString(digest[:]), OrganizationID: item.OrganizationID, IdentityID: item.Applicant, ProjectID: item.ProjectID, Period: effect.OccurredAt[:7], Status: "POSTED", Currency: claim.Currency, AmountMinor: item.AmountMinor * sign}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
