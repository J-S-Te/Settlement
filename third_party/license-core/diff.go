package licensecore

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// Digest authenticates the token before returning its exact compact JWS digest.
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

// Diff compares validated, same-binding, strictly increasing license versions.
// Returned snapshots are independent copies, sorted by application code.
// The caller must verify both licenses before comparing them.
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
