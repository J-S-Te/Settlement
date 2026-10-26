package service

import (
	"fmt"
	"math/big"
)

func normalizeInvoiceItems(items []InvoiceItemInput) ([]InvoiceItemInput, *big.Rat, *big.Rat, *big.Rat, error) {
	excl, tax, incl := new(big.Rat), new(big.Rat), new(big.Rat)
	normalized := make([]InvoiceItemInput, 0, len(items))
	for _, item := range items {
		q, err := decimal(item.Quantity, true)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		price, err := decimal(item.UnitPriceExclTax, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ae, err := amount(item.AmountExclTax)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		tr, err := decimal(item.TaxRate, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ta, err := nonNegativeAmount(item.TaxAmount)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ai, err := amount(item.AmountInclTax)
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
		if ae != expectedExcl || ta != expectedTax || ai != expectedIncl {
			return nil, nil, nil, nil, fmt.Errorf("%w: invoice line does not match quantity, unit price, tax rate and totals", ErrInvalid)
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
