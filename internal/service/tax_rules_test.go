package service

import (
	"errors"
	"testing"
)

// TestConfiguredTaxRates 用途：验证税率配置实际约束申请，数值等价表示兼容。
// 参数：t为测试上下文。
// 返回值：无，未配置税率被接受或等价税率被拒绝时失败。
func TestConfiguredTaxRates(t *testing.T) {
	if err := validateConfiguredTaxRates([]InvoiceItemInput{{TaxRate: "0.10"}}, []string{"0.06"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := validateConfiguredTaxRates([]InvoiceItemInput{{TaxRate: "0.060000"}}, []string{"0.06"}); err != nil {
		t.Fatal(err)
	}
}
