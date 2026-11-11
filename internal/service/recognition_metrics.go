package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

type RecognitionMetricRow struct {
	UnknownReason    string `json:"unknown_reason,omitempty"`
	Kind             string `json:"kind"`
	RecordID         string `json:"record_id"`
	OrganizationID   string `json:"organization_id"`
	IdentityID       string `json:"identity_id"`
	ProjectID        string `json:"project_id"`
	Period           string `json:"period"`
	Status           string `json:"status"`
	OrganizationName string `json:"organization_name,omitempty"`
	IdentityName     string `json:"identity_name,omitempty"`
	ProjectName      string `json:"project_name,omitempty"`
	Currency         string `json:"currency"`
	AmountMinor      int64  `json:"amount_minor"`
	TaxMinor         int64  `json:"tax_minor"`
	CostCategory     string `json:"cost_category,omitempty"`
}
type RecognitionMetrics struct {
	SchemaVersion       int                    `json:"schema_version"`
	TenantID            string                 `json:"tenant_id"`
	SourceCode          string                 `json:"source_code"`
	SnapshotAt          string                 `json:"snapshot_at"`
	SourceUpdatedAt     string                 `json:"source_updated_at,omitempty"`
	CollectionStartedAt string                 `json:"collection_started_at,omitempty"`
	Complete            bool                   `json:"complete"`
	Basis               string                 `json:"basis"`
	Rows                []RecognitionMetricRow `json:"rows"`
}

// DashboardMetrics 在单RR只读事务采集已登记确认，现金来源完整读取后单独列示。
// 参数：ctx为读取上下文，tenant为机器签名租户。
// 返回值：有界完整指标；来源失败、数据库或提交失败则整体失败。
func (s *Service) DashboardMetrics(ctx context.Context, tenant string) (RecognitionMetrics, error) {
	out := RecognitionMetrics{SchemaVersion: 1, TenantID: tenant, SourceCode: "settlement", SnapshotAt: time.Now().UTC().Format(time.RFC3339Nano), Complete: false, Basis: "本系统已登记且财务独立复核的不含税净确认；冲回按本次确认月列负数；现金/预付独立列示；旧历史不猜填", Rows: []RecognitionMetricRow{}}
	if tenant == "" || s.DB == nil {
		return out, ErrDependency
	}
	var source CostSnapshot
	if s.ReimbursementCosts != nil {
		var e error
		source, e = s.ReimbursementCosts.ReadCosts(ctx, tenant, "")
		if e != nil {
			return out, ErrDependency
		}
		if e = validateCostSnapshot(source, tenant, s.RecognitionEnvironment, ""); e != nil {
			return out, e
		}
	} else {
		out.Basis += "；报销现金来源未配置，未声明已接入"
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	out.SnapshotAt = time.Now().UTC().Format(time.RFC3339Nano)
	records, e := recognitionRows(ctx, tx, tenant)
	if e != nil {
		return out, e
	}
	for _, record := range records {
		if out.CollectionStartedAt == "" || record.CreatedAt.Before(parseMetricTime(out.CollectionStartedAt)) {
			out.CollectionStartedAt = record.CreatedAt.UTC().Format(time.RFC3339Nano)
		}
		if out.SourceUpdatedAt == "" || record.UpdatedAt.After(parseMetricTime(out.SourceUpdatedAt)) {
			out.SourceUpdatedAt = record.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
		row := recognitionMetric(record)
		switch record.Status {
		case "CONFIRMED":
			if record.Kind == "INCOME" {
				row.Kind = "recognized_income"
			} else {
				row.Kind = "recognized_cost"
			}
		case "SUBMITTED":
			row.Kind = "recognition_pending"
			row.Status = "PENDING"
			row.AmountMinor = 0
			row.TaxMinor = 0
		default:
			continue
		}
		out.Rows = append(out.Rows, row)
	}
	for _, claim := range source.Claims {
		for _, record := range recognitionConflicts(claim, s.RecognitionEnvironment, records) {
			row := recognitionMetric(record)
			row.Kind = "recognition_conflict"
			row.RecordID = "refund:" + record.ID
			row.Status = "CONFLICT"
			row.AmountMinor = 0
			row.TaxMinor = 0
			out.Rows = append(out.Rows, row)
		}
		for _, effect := range claim.Effects {
			if recorded := parseMetricTime(effect.RecordedAt); out.SourceUpdatedAt == "" || recorded.After(parseMetricTime(out.SourceUpdatedAt)) {
				out.SourceUpdatedAt = recorded.UTC().Format(time.RFC3339Nano)
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
				out.Rows = append(out.Rows, RecognitionMetricRow{Kind: "reimbursement_unknown", RecordID: claim.ClaimID + ":" + effect.EffectID, Period: effect.OccurredAt[:7], Status: "UNKNOWN", Currency: claim.Currency, AmountMinor: value, UnknownReason: reason})
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
				out.Rows = append(out.Rows, RecognitionMetricRow{Kind: kind, RecordID: hex.EncodeToString(digest[:]), OrganizationID: item.OrganizationID, IdentityID: item.Applicant, ProjectID: item.ProjectID, Period: effect.OccurredAt[:7], Status: "POSTED", Currency: claim.Currency, AmountMinor: item.AmountMinor * sign})
			}
		}
	}
	if len(out.Rows) > 10000 {
		return out, fmt.Errorf("%w: 指标超过10000行", ErrDependency)
	}
	raw, e := json.Marshal(out)
	if e != nil || len(raw) > (8<<20)-64 {
		return out, ErrDependency
	}
	if e = tx.Commit(); e != nil {
		return out, e
	}
	out.Complete = true
	return out, nil
}

// parseMetricTime 解析仅由本方法产生的UTC时间，供最早及最后事实时间比较。
// 参数：raw为已生成RFC3339Nano时间。
// 返回值：解析时间；空字符串返回零值。
func parseMetricTime(raw string) time.Time { at, _ := time.Parse(time.RFC3339Nano, raw); return at }

// recognitionMetric 从冻结维度转换指标；冲回在其本次确认期间列负。
// 参数：record为真实确认或待审事实。
// 返回值：不包含凭证与业务说明的最小维度行。
func recognitionMetric(record Recognition) RecognitionMetricRow {
	sign := int64(1)
	if record.OriginalID != "" {
		sign = -1
	}
	return RecognitionMetricRow{RecordID: record.ID, OrganizationID: record.OrganizationID, IdentityID: record.IdentityID, ProjectID: record.ProjectID, Period: record.RecognitionDate[:7], Status: record.Status, OrganizationName: record.OrganizationName, IdentityName: record.IdentityName, ProjectName: record.ProjectName, Currency: record.Currency, AmountMinor: record.NetMinor * sign, TaxMinor: record.TaxMinor * sign, CostCategory: record.CostCategory}
}
