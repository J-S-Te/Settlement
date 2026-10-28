package service

import (
	"fmt"
	"math/big"
)

// validateConfiguredTaxRates 用途：限制开票申请只能使用管理员配置的有效税率。
// 参数：items为已规范化票面明细，rates为已校验配置税率。
// 返回值：全部匹配时无错误，否则返回参数错误，不修改财务事实。
func validateConfiguredTaxRates(items []InvoiceItemInput, rates []string) error {
	for _, item := range items {
		value, ok := new(big.Rat).SetString(item.TaxRate)
		if !ok {
			return ErrInvalid
		}
		allowed := false
		for _, rate := range rates {
			candidate, valid := new(big.Rat).SetString(rate)
			if valid && value.Cmp(candidate) == 0 {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("%w: tax_rate is not configured for this tenant", ErrInvalid)
		}
	}
	return nil
}
