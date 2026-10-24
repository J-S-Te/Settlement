package licensecore

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// Digest 先验证令牌，再返回原始紧凑格式 JWS 的摘要。
func Digest(raw string, trusted map[string]ed25519.PublicKey) (string, error) {
	if _, err := Verify(raw, trusted); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:]), nil
}

type ApplicationChange struct {
	Code string       `json:"code"`
	Kind string       `json:"kind"`
	Old  *Application `json:"old,omitempty"`
	New  *Application `json:"new,omitempty"`
}

const (
	ADDED   = "ADDED"
	REMOVED = "REMOVED"
	UPDATED = "UPDATED"
)

// Diff 比较已验证、绑定相同且版本严格递增的两份授权。
// 返回按应用编码排序的独立快照副本。
// 调用方须先验证两份授权，再进行比较。
func Diff(next, current License) ([]ApplicationChange, error) {
	if err := CompareVersions(next, current); err != nil {
		return nil, err
	}
	before := map[string]Application{}
	after := map[string]Application{}
	codes := map[string]bool{}
	for _, a := range current.Applications {
		before[a.Code] = a
		codes[a.Code] = true
	}
	for _, a := range next.Applications {
		after[a.Code] = a
		codes[a.Code] = true
	}
	ordered := make([]string, 0, len(codes))
	for code := range codes {
		ordered = append(ordered, code)
	}
	sort.Strings(ordered)
	result := make([]ApplicationChange, 0)
	for _, code := range ordered {
		old, hasOld := before[code]
		fresh, hasNew := after[code]
		switch {
		case !hasOld:
			result = append(result, ApplicationChange{Code: code, Kind: ADDED, New: &fresh})
		case !hasNew:
			result = append(result, ApplicationChange{Code: code, Kind: REMOVED, Old: &old})
		case old != fresh:
			result = append(result, ApplicationChange{Code: code, Kind: UPDATED, Old: &old, New: &fresh})
		}
	}
	return result, nil
}
