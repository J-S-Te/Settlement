package service

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/j-s-te/settlement/internal/businessclock"
)

// 发票类型取值。集合来自前端 settlementDisplay.invoiceTypeLabel 使用的四个
// 展示值（电子/纸质 × 普通/专用）。VAT_NORMAL/VAT_SPECIAL 是既有集成与回归
// 夹具（integration/invoice_flow_test.go、role_finance_mysql_test.go 等）仍在使用的
// 历史取值，暂按兼容集合保留；是否收紧为仅四个电子/纸质取值需产品确认。
const (
	InvoiceTypeElectronicNormal  = "ELECTRONIC_NORMAL"
	InvoiceTypeElectronicSpecial = "ELECTRONIC_SPECIAL"
	InvoiceTypePaperNormal       = "PAPER_NORMAL"
	InvoiceTypePaperSpecial      = "PAPER_SPECIAL"
	InvoiceTypeVATNormal         = "VAT_NORMAL"
	InvoiceTypeVATSpecial        = "VAT_SPECIAL"
)

// invoiceTypeLimit 与 settlement_invoice_request.invoice_type VARCHAR(32) 对齐。
const invoiceTypeLimit = 32

// invoiceProfileMaxName/invoiceProfileMaxTaxNo/invoiceProfileMaxBankAccount 等
// 与快照列宽对齐：customer_name_snapshot VARCHAR(255)、customer_id VARCHAR(128)。
// 发票要素字符串长度上限按“字符数”计，避免多字节中文被字节数误判。
const (
	profileMaxName        = 255
	profileMaxCustomerID  = 128
	profileMaxTaxNo       = 64
	profileMaxAddress     = 255
	profileMaxPhone       = 64
	profileMaxBankName    = 255
	profileMaxBankAccount = 64

	invoiceDocumentNumberLimit = 32
)

// invoiceTypeValues 是唯一合法的发票类型集合，查表即白名单。
var invoiceTypeValues = map[string]struct{}{
	InvoiceTypeElectronicNormal:  {},
	InvoiceTypeElectronicSpecial: {},
	InvoiceTypePaperNormal:       {},
	InvoiceTypePaperSpecial:      {},
	InvoiceTypeVATNormal:         {},
	InvoiceTypeVATSpecial:        {},
}

// buyerProfileFields 定义买方档案字段白名单及其长度上限。
// 字段集合按当前前端 useInvoiceRequests.submitInvoice 实际提交
// （customer_id/name/tax_no）与人工/税控实际需要的抬头要素确定；
// 未列入的键一律拒绝，避免客户端把任意 JSON 写入
// settlement_invoice_request.buyer_profile_snapshot 并原样透传给税控。
var buyerProfileFields = map[string]int{
	"customer_id":  profileMaxCustomerID,
	"name":         profileMaxName,
	"tax_no":       profileMaxTaxNo,
	"address":      profileMaxAddress,
	"phone":        profileMaxPhone,
	"bank_name":    profileMaxBankName,
	"bank_account": profileMaxBankAccount,
}

// sellerProfileFields 定义卖方档案字段白名单及其长度上限。
// 卖方不含 customer_id：销售方不是本租户客户；销售方要素直接来自开票方，
// 不参与客户一致性校验。
var sellerProfileFields = map[string]int{
	"name":         profileMaxName,
	"tax_no":       profileMaxTaxNo,
	"address":      profileMaxAddress,
	"phone":        profileMaxPhone,
	"bank_name":    profileMaxBankName,
	"bank_account": profileMaxBankAccount,
}

// invoiceDocumentNumberPattern 限制票号只含字母、数字与连字符，长度由调用方限定。
var invoiceDocumentNumberPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// invoiceTypeStoredPattern 限制库内发票类型形态：大写字母开头，后接大写/数字/下划线。
var invoiceTypeStoredPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// invoiceIssueDateLayout 是票面开票日期的唯一接受格式。
const invoiceIssueDateLayout = "2006-01-02"

// normalizeInvoiceType 校验并按大写归一化发票类型。
// 参数：value 为客户端或库内的发票类型；allowed 隐含服务端白名单。
// 返回值：归一化后的类型；非法（空、超长、不在白名单）返回 ErrInvalid。
func normalizeInvoiceType(value string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	if normalized == "" {
		return "", fmt.Errorf("%w: invoice_type is required", ErrInvalid)
	}
	if len(normalized) > invoiceTypeLimit {
		return "", fmt.Errorf("%w: invoice_type exceeds the length limit", ErrInvalid)
	}
	if _, ok := invoiceTypeValues[normalized]; !ok {
		return "", fmt.Errorf("%w: invoice_type %q is not supported", ErrInvalid, normalized)
	}
	return normalized, nil
}

// validateStoredInvoiceType 校验库内已落库的发票类型形态，用于出站税控命令。
// 参数：value 为 settlement_invoice_request.invoice_type 的库内值。
// 返回值：归一化类型；为空、超长或含非法字符返回 ErrInvalid。
// 与 normalizeInvoiceType 的区别：这里不按新版枚举收口，避免历史合法取值
// （如旧夹具写入的 SPECIAL）在开具阶段被一刀切挡住，但仍禁止任意文本出站。
func validateStoredInvoiceType(value string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	if normalized == "" {
		return "", fmt.Errorf("%w: invoice_type is required", ErrInvalid)
	}
	if len(normalized) > invoiceTypeLimit || !invoiceTypeStoredPattern.MatchString(normalized) {
		return "", fmt.Errorf("%w: invoice_type %q is not a valid tax invoice type", ErrInvalid, normalized)
	}
	return normalized, nil
}

// normalizeInvoiceProfile 校验并归一化买方/卖方档案。
// 参数：profile 为客户端提交的档案；allowed 为该角色的字段白名单；label 用于错误文案。
// 返回值：去空白后的新档案（nil 输入返回空档案，不再写入 JSON null）；
// 未知键、非字符串值、超长或含控制字符均返回 ErrInvalid。
func normalizeInvoiceProfile(profile map[string]any, allowed map[string]int, label string) (map[string]any, error) {
	if len(profile) == 0 {
		return map[string]any{}, nil
	}
	normalized := make(map[string]any, len(profile))
	for key, value := range profile {
		limit, ok := allowed[key]
		if !ok {
			// 未知键必须拒绝：否则任意 JSON 会原样入库并透传给税控。
			return nil, fmt.Errorf("%w: %s field %q is not allowed", ErrInvalid, label, key)
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s field %q must be a string", ErrInvalid, label, key)
		}
		text = strings.TrimSpace(text)
		if textTooLong(text, limit) {
			return nil, fmt.Errorf("%w: %s field %q exceeds the length limit", ErrInvalid, label, key)
		}
		if strings.IndexFunc(text, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("%w: %s field %q contains control characters", ErrInvalid, label, key)
		}
		normalized[key] = text
	}
	return normalized, nil
}

// requireBuyerProfileCustomer 校验买方档案的客户标识与合同快照客户一致。
// 参数：profile 为已归一化的买方档案；snapshotCustomer 为合同快照的 customer_id。
// 返回值：未提供 customer_id 时不校验；提供了但不等于快照客户返回 ErrInvalid。
func requireBuyerProfileCustomer(profile map[string]any, snapshotCustomer string) error {
	value, ok := profile["customer_id"]
	if !ok {
		return nil
	}
	customerID, _ := value.(string)
	if customerID == "" {
		// 空客户标识等同于未提供，避免用空串绕过一致性校验。
		return nil
	}
	if customerID != strings.TrimSpace(snapshotCustomer) {
		return fmt.Errorf("%w: buyer_profile customer_id does not match the contract customer", ErrInvalid)
	}
	return nil
}

// validateInvoiceDocumentNumber 校验发票代码/号码的长度与字符白名单。
// 参数：value 为已去空白的票号；name 为字段名（用于错误文案）。
// 返回值：合法返回 nil；为空、超长或含白名单外字符返回 ErrInvalid。
func validateInvoiceDocumentNumber(value, name string) error {
	if value == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalid, name)
	}
	if len([]rune(value)) > invoiceDocumentNumberLimit {
		return fmt.Errorf("%w: %s exceeds the length limit", ErrInvalid, name)
	}
	if !invoiceDocumentNumberPattern.MatchString(value) {
		return fmt.Errorf("%w: %s may only contain letters, digits and hyphens", ErrInvalid, name)
	}
	return nil
}

// validateInvoiceIssueDateOn 以给定业务日判定开票日期是否合法。
// 参数：value 为 YYYY-MM-DD 文本；today 为北京业务日（00:00，Location=UTC 的日历日）。
// 返回值：归一化日期；格式错误或晚于业务日返回 ErrInvalid。
// 该内核不读时钟，便于单测覆盖“未来日期”分支。
func validateInvoiceIssueDateOn(value string, today time.Time) (string, error) {
	normalized := strings.TrimSpace(value)
	day, err := time.Parse(invoiceIssueDateLayout, normalized)
	if err != nil {
		return "", fmt.Errorf("%w: issue_date must be a calendar date like 2006-01-02", ErrInvalid)
	}
	// 两端都是“北京业务日”编码（00:00 + Location=UTC），按自然日比较，
	// 不能用 UTC 当前时间直接比较，否则北京 00:00-08:00 会误判为未来。
	if day.After(today) {
		return "", fmt.Errorf("%w: issue_date must not be later than the Beijing business day", ErrInvalid)
	}
	return normalized, nil
}

// validateInvoiceIssueDate 用当前北京业务日校验票面开票日期。
// 参数：value 为 YYYY-MM-DD 文本；
// 返回值：归一化日期或 ErrInvalid。
func validateInvoiceIssueDate(value string) (string, error) {
	return validateInvoiceIssueDateOn(value, businessclock.Today())
}
