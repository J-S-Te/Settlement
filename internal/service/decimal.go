package service

import (
	"fmt"
	"math/big"
	"strings"
)

func decimal(value string, positive bool) (string, error) {
	value = strings.TrimSpace(value)
	if !sixDecimalPattern.MatchString(value) {
		return "", fmt.Errorf("%w: invalid decimal", ErrInvalid)
	}
	r, ok := new(big.Rat).SetString(value)
	if !ok || r.Sign() < 0 || (positive && r.Sign() == 0) {
		return "", fmt.Errorf("%w: invalid decimal", ErrInvalid)
	}
	return r.FloatString(6), nil
}
func add(values ...string) *big.Rat {
	sum := new(big.Rat)
	for _, value := range values {
		r, _ := new(big.Rat).SetString(value)
		sum.Add(sum, r)
	}
	return sum
}

// incrementInvoiced 锁定应收，并写入明确的新已开票
// 余额。不能使用 UPDATE 正在修改的同一列计算状态，
// 因为 MySQL 以已更新行求值后续赋值。
