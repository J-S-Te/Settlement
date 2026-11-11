package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const RecognitionMaximum int64 = 9007199254740991

type RecognitionInput struct {
	Kind                  string `json:"kind"`
	CostCategory          string `json:"cost_category"`
	OrganizationID        string `json:"organization_id"`
	OrganizationWitnessID string `json:"organization_witness_id"`
	IdentityID            string `json:"identity_id,omitempty"`
	ProjectID             string `json:"project_id,omitempty"`
	ServiceItemID         string `json:"service_item_id,omitempty"`
	ContractSnapshotID    string `json:"contract_snapshot_id,omitempty"`
	Currency              string `json:"currency"`
	AmountMinor           int64  `json:"amount_minor"`
	TaxMinor              int64  `json:"tax_minor"`
	TaxBasis              string `json:"tax_basis"`
	OccurredDate          string `json:"occurred_date"`
	RecognitionDate       string `json:"recognition_date"`
	EvidenceReference     string `json:"evidence_reference"`
	Note                  string `json:"note"`
	SourceSystem          string `json:"source_system"`
	SourceEnvironment     string `json:"source_environment"`
	SourceFactID          string `json:"source_fact_id"`
	SourceClaimID         string `json:"source_claim_id,omitempty"`
	SourceEffectID        string `json:"source_effect_id,omitempty"`
	SourceItemID          string `json:"source_item_id,omitempty"`
	SourceAllocationKey   string `json:"source_allocation_key,omitempty"`
	PrepaymentBasis       string `json:"prepayment_basis,omitempty"`
	OriginalID            string `json:"original_id,omitempty"`
}
type Recognition struct {
	RecognitionInput
	ID               string     `json:"id"`
	TenantID         string     `json:"tenant_id"`
	OrganizationName string     `json:"organization_name"`
	IdentityName     string     `json:"identity_name,omitempty"`
	ProjectName      string     `json:"project_name,omitempty"`
	NetMinor         int64      `json:"net_minor"`
	GrossMinor       int64      `json:"gross_minor"`
	Status           string     `json:"status"`
	Version          uint64     `json:"version"`
	CreatedBy        string     `json:"created_by"`
	SubmittedBy      string     `json:"submitted_by,omitempty"`
	ReviewedBy       string     `json:"reviewed_by,omitempty"`
	ReviewNote       string     `json:"review_note,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	ReviewedAt       *time.Time `json:"reviewed_at,omitempty"`
}
type RecognitionDecision struct {
	ExpectedVersion uint64 `json:"expected_version"`
	Decision        string `json:"decision,omitempty"`
	Note            string `json:"note,omitempty"`
}
type RecognitionOrganization struct {
	ID   string `json:"organization_id"`
	Name string `json:"organization_name"`
}
type RecognitionPerson struct {
	ID            string                    `json:"user_id"`
	Name          string                    `json:"display_name"`
	Organizations []RecognitionOrganization `json:"organizations"`
}
type RecognitionDirectory interface {
	RecognitionPeople(context.Context, string, string) ([]RecognitionPerson, error)
}

// recognitionAllowed 显式绑定动作、实际角色和可信组织或租户范围，不走旧全量兼容。
// 参数：p为可信当前主体，permission为此动作，org为冻结承担组织，空组织仅检查存在有效范围。
// 返回值：有明确授权及范围才为真；管理员仍受同租户及职责分离检查。
func recognitionAllowed(p Principal, permission, org string) bool {
	if p.TenantID == "" || p.UserID == "" || p.CatalogVersion == "" || !p.Permissions[permission] {
		return false
	}
	for _, role := range p.Roles {
		if !catalogRoleGrants[role][permission] {
			continue
		}
		for _, scope := range p.DataScopes {
			if scope.RoleCode != role {
				continue
			}
			if scope.ScopeType == "TENANT" {
				return true
			}
			if scope.ScopeType == "ORG" && scope.ScopeID != "" && (org == "" || scope.ScopeID == org) {
				return true
			}
		}
	}
	return false
}

// validateRecognition 校验定点金额、明确税基、独立发生日与确认日及真实来源标识。
// 参数：in为草稿业务输入，now为服务器时间。
// 返回值：合法时无错误；金额或来源不完整返回中文参数错误。
func validateRecognition(in RecognitionInput, now time.Time) error {
	invalid := func(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }
	if in.Kind != "INCOME" && in.Kind != "COST" {
		return invalid("请选择收入或成本")
	}
	if in.Kind == "COST" && !strings.Contains("|LABOR|PROCUREMENT|EQUIPMENT|REIMBURSEMENT|OTHER|", "|"+in.CostCategory+"|") {
		return invalid("请选择真实成本类别")
	}
	if in.Kind == "INCOME" && in.CostCategory != "" {
		return invalid("收入不能指定成本类别")
	}
	if in.AmountMinor < 0 || in.AmountMinor > RecognitionMaximum || in.TaxMinor < 0 || in.TaxMinor > RecognitionMaximum {
		return invalid("金额与税额须为安全范围内的非负整数分")
	}
	if in.TaxBasis != "EXCLUSIVE" && in.TaxBasis != "INCLUSIVE" {
		return invalid("必须明确含税或未税口径")
	}
	if in.TaxBasis == "INCLUSIVE" && in.TaxMinor > in.AmountMinor || in.TaxBasis == "EXCLUSIVE" && in.AmountMinor > RecognitionMaximum-in.TaxMinor {
		return invalid("税额与金额不守恒")
	}
	net, _ := recognitionAmounts(in)
	if net == 0 && (in.OriginalID == "" || in.TaxMinor <= 0) {
		return invalid("新确认须有正净额；仅关联原确认的冲回可登记零净额正税额")
	}
	if !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(in.Currency) {
		return invalid("币种须为三位大写代码")
	}
	today := now.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	for _, date := range []string{in.OccurredDate, in.RecognitionDate} {
		at, e := time.Parse("2006-01-02", date)
		if e != nil || at.Format("2006-01-02") != date || date > today {
			return invalid("发生日及确认日须为有效的已发生北京业务日")
		}
	}
	for _, value := range []string{in.OrganizationID, in.OrganizationWitnessID, in.EvidenceReference, in.Note, in.SourceSystem, in.SourceEnvironment, in.SourceFactID} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || len(value) > 2000 || strings.ContainsRune(value, '\x00') {
			return invalid("请补齐组织、真实凭证、说明及稳定来源身份")
		}
	}
	for _, value := range []string{in.OrganizationID, in.OrganizationWitnessID, in.IdentityID, in.ProjectID, in.ServiceItemID, in.ContractSnapshotID, in.EvidenceReference, in.SourceSystem, in.SourceEnvironment, in.SourceFactID, in.SourceClaimID, in.SourceEffectID, in.SourceItemID, in.SourceAllocationKey, in.OriginalID} {
		if len(value) > 128 {
			return invalid("引用标识过长")
		}
	}
	if in.ServiceItemID != "" && in.ProjectID == "" {
		return invalid("服务项须关联权威项目")
	}
	if in.SourceSystem != "manual" && in.SourceSystem != "reimbursement" {
		return invalid("来源须为人工登记或权威报销候选")
	}
	if in.SourceSystem == "reimbursement" && (in.Kind != "COST" || in.SourceClaimID == "" || in.SourceEffectID == "" || in.SourceItemID == "" || in.SourceAllocationKey == "") {
		return invalid("报销候选身份不完整")
	}
	if in.SourceClaimID != "" && in.SourceSystem != "reimbursement" {
		return invalid("关联报销单须使用权威费用候选")
	}
	return nil
}

// recognitionAmounts 将明确税基转换为独立的不含税净额及含税上限。
// 参数：in为已校验的金额输入。
// 返回值：未税净额及含税金额，均为最小币种单位。
func recognitionAmounts(in RecognitionInput) (int64, int64) {
	if in.TaxBasis == "INCLUSIVE" {
		return in.AmountMinor - in.TaxMinor, in.AmountMinor
	}
	return in.AmountMinor, in.AmountMinor + in.TaxMinor
}

// verifyRecognitionDirectory 用精确在职人员证明组织存在并冻结当前选择名称。
// 参数：ctx/p为授权上下文，in为显式组织/人员归属。
// 返回值：组织与人员显示名；依赖缺失或引用失效返回错误。
func (s *Service) verifyRecognitionDirectory(ctx context.Context, p Principal, in RecognitionInput) (string, string, error) {
	if s.RecognitionDirectory == nil {
		return "", "", ErrDependency
	}
	people, err := s.RecognitionDirectory.RecognitionPeople(ctx, in.OrganizationWitnessID, "")
	if err != nil {
		return "", "", ErrDependency
	}
	if len(people) != 1 || people[0].ID != in.OrganizationWitnessID {
		return "", "", ErrInvalid
	}
	orgName := ""
	for _, org := range people[0].Organizations {
		if org.ID == in.OrganizationID {
			orgName = org.Name
		}
	}
	if orgName == "" {
		return "", "", ErrInvalid
	}
	name := ""
	if in.IdentityID != "" {
		rows, e := s.RecognitionDirectory.RecognitionPeople(ctx, in.IdentityID, "")
		if e != nil {
			return "", "", ErrDependency
		}
		if len(rows) != 1 || rows[0].ID != in.IdentityID {
			return "", "", ErrInvalid
		}
		if !recognitionPersonVisible(p, rows[0]) {
			return "", "", ErrForbidden
		}
		name = rows[0].Name
	}
	return orgName, name, nil
}

// recognitionJSON 编码完整确认事实，避免数据库列和返回对象各自拼接造成漂移。
// 参数：record为当前完整事实。
// 返回值：规范结构JSON及编码错误。
func recognitionJSON(record Recognition) ([]byte, error) { return json.Marshal(record) }

// RecognitionOrganizationAllowed 判断目录组织是否属于当前明确财务读取范围。
// 参数：p为可信主体，org为受控目录组织。
// 返回值：组织可见才为真，不采用legacy范围兼容。
func RecognitionOrganizationAllowed(p Principal, org string) bool {
	return recognitionAllowed(p, "settlement.recognition.read", org)
}

// recognitionPersonVisible 复用目录候选的可信读取范围核对精确人员，允许显式有权的跨组织归属。
// 参数：p为当前可信主体，person为受控目录精确返回的在职人员。
// 返回值：人员至少一个组织在当前recognition.read范围内才为真。
func recognitionPersonVisible(p Principal, person RecognitionPerson) bool {
	for _, org := range person.Organizations {
		if RecognitionOrganizationAllowed(p, org.ID) {
			return true
		}
	}
	return false
}
