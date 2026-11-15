package service

import (
	"fmt"
	"math/big"
)

// 用途：按既有精度核对发票明细，并在拒绝时保留真实行号与字段。
// 参数：items 为本次申请明细，不改变数量、税率、舍入或零税额规则。
// 返回值：归一化明细及三项合计；字段或关系错误保留 ErrInvalid。
func normalizeInvoiceItems(items []InvoiceItemInput) ([]InvoiceItemInput, *big.Rat, *big.Rat, *big.Rat, error) {
	excl, tax, incl := new(big.Rat), new(big.Rat), new(big.Rat)
	normalized := make([]InvoiceItemInput, 0, len(items))
	for index, item := range items {
		row := index + 1
		q, err := invoiceFieldValue(row, "quantity", "数量", item.Quantity, false, true)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		price, err := invoiceFieldValue(row, "unit_price_excl_tax", "不含税单价", item.UnitPriceExclTax, false, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ae, err := invoiceFieldValue(row, "amount_excl_tax", "不含税金额", item.AmountExclTax, true, true)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		tr, err := invoiceFieldValue(row, "tax_rate", "税率", item.TaxRate, false, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ta, err := invoiceFieldValue(row, "tax_amount", "税额", item.TaxAmount, true, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ai, err := invoiceFieldValue(row, "amount_incl_tax", "价税合计", item.AmountInclTax, true, true)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		quantity, _ := new(big.Rat).SetString(q)
		unitPrice, _ := new(big.Rat).SetString(price)
		expectedExcl := new(big.Rat).Mul(quantity, unitPrice).FloatString(2)
		taxRate, _ := new(big.Rat).SetString(tr)
		expectedExclRat, _ := new(big.Rat).SetString(expectedExcl)
		expectedTax := new(big.Rat).Mul(expectedExclRat, taxRate).FloatString(2)
		expectedIncl := add(expectedExcl, expectedTax).FloatString(2)
		if ae != expectedExcl {
			return nil, nil, nil, nil, invoiceRelationError(row, "amount_excl_tax", "不含税金额", expectedExcl, "数量 × 不含税单价")
		}
		if ta != expectedTax {
			return nil, nil, nil, nil, invoiceRelationError(row, "tax_amount", "税额", expectedTax, "不含税金额 × 税率")
		}
		if ai != expectedIncl {
			return nil, nil, nil, nil, invoiceRelationError(row, "amount_incl_tax", "价税合计", expectedIncl, "不含税金额 + 税额")
		}
		item.Quantity = q
		item.UnitPriceExclTax = price
		item.AmountExclTax = ae
		item.TaxRate = tr
		item.TaxAmount = ta
		item.AmountInclTax = ai
		normalized = append(normalized, item)
		excl.Add(excl, add(ae))
		tax.Add(tax, add(ta))
		incl.Add(incl, add(ai))
	}
	return normalized, excl, tax, incl, nil
}

// 用途：复用现有数值校验并只公开固定字段说明，不回显被拒绝的输入。
// 参数：row 为一基行号，field/label 为固定字段，value 为输入，money/positive 为原校验口径。
// 返回值：归一化数值；非法输入返回可定位字段的安全 ErrInvalid。
func invoiceFieldValue(row int, field, label, value string, money, positive bool) (string, error) {
	var normalized string
	var err error
	if money {
		if positive {
			normalized, err = amount(value)
		} else {
			normalized, err = nonNegativeAmount(value)
		}
	} else {
		normalized, err = decimal(value, positive)
	}
	if err == nil {
		return normalized, nil
	}
	precision, bound := "六", "大于等于 0"
	if money {
		precision = "两"
	}
	if positive {
		bound = "大于 0"
	}
	detail := fmt.Sprintf("%s格式不正确，应%s且最多保留%s位小数；请核对开票明细", label, bound, precision)
	message := fmt.Sprintf("第 %d 行%s", row, detail)
	return "", businessFeedback(ErrInvalid, "INVOICE_LINE_VALUE", message,
		feedbackIssue{Field: fmt.Sprintf("items[%d].%s", row-1, field), Row: row, Rule: "invalid", Message: detail})
}

// 用途：说明既有票面关系的计算结果，不要求用户修改页面自动生成字段。
// 参数：row 为行号，field/label 为固定字段，expected 为按原舍入计算的金额，relation 为关系说明。
// 返回值：包含真实行号、字段及安全金额关系的 ErrInvalid。
func invoiceRelationError(row int, field, label, expected, relation string) error {
	detail := fmt.Sprintf("%s应为 %s（%s，按两位小数计算），请核对该行金额关系", label, expected, relation)
	message := fmt.Sprintf("第 %d 行%s", row, detail)
	return businessFeedback(ErrInvalid, "INVOICE_LINE_RELATION", message,
		feedbackIssue{Field: fmt.Sprintf("items[%d].%s", row-1, field), Row: row, Rule: "mismatch", Message: detail})
}
