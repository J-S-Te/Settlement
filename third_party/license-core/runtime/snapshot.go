// Package runtime provides local, durable commercial-license gates. Callers
// must authenticate and authorize the original operation before invoking it.
package runtime

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	core "github.com/J-S-Te/license-core"
)

const SnapshotType = "platform-runtime-snapshot+jws"
const Protocol = 1
const maxBytes = 1024 * 1024
const maxTime int64 = 253402300799

type EnforcementState string

const (
	Pending  EnforcementState = "PENDING_ENFORCEMENT"
	Applying EnforcementState = "APPLYING"
	Enforced EnforcementState = "ENFORCED"
)

var (
	ErrInvalid  = errors.New("invalid runtime snapshot or state")
	ErrRollback = errors.New("runtime rollback rejected")
	ErrState    = errors.New("runtime durable state unavailable")
	ErrClock    = errors.New("runtime clock recovery required")
)

type Binding struct {
	InstanceID  string `json:"instanceID"`
	Environment string `json:"environment"`
	Application string `json:"application"`
	ServiceID   string `json:"serviceID"`
}

type PlatformSnapshot struct {
	Protocol              int              `json:"protocol"`
	InstanceID            string           `json:"instanceID"`
	Environment           string           `json:"environment"`
	Application           string           `json:"application"`
	ServiceID             string           `json:"serviceID"`
	Revision              uint64           `json:"revision"`
	EnforcementState      EnforcementState `json:"enforcementState"`
	MigrationEligible     bool             `json:"migrationEligible"`
	CurrentLicenseJWS     string           `json:"currentLicenseJWS"`
	PendingLicenseJWS     string           `json:"pendingLicenseJWS"`
	HighestLicenseVersion uint64           `json:"highestLicenseVersion"`
	SnapshotIssuedAt      int64            `json:"snapshotIssuedAt"`
}

type header struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

func identifier(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return false
		}
	}
	return true
}
func validTime(t int64) bool { return t > 0 && t <= maxTime }
func validBinding(b Binding) bool {
	if !identifier(b.InstanceID) || !identifier(b.Environment) || !identifier(b.ServiceID) {
		return false
	}
	switch b.Application {
	case "customer_and_opportunity", "customer_portal", "contract_management", "project_management", "settlement", "data_analysis":
		return true
	}
	return false
}
func rank(s EnforcementState) int {
	switch s {
	case Pending:
		return 0
	case Applying:
		return 1
	case Enforced:
		return 2
	}
	return -1
}
func (s PlatformSnapshot) Binding() Binding {
	return Binding{s.InstanceID, s.Environment, s.Application, s.ServiceID}
}
func validateSnapshot(s PlatformSnapshot) error {
	if s.Protocol != Protocol || !validBinding(s.Binding()) || s.Revision == 0 || rank(s.EnforcementState) < 0 || !validTime(s.SnapshotIssuedAt) || (s.MigrationEligible && s.EnforcementState != Pending) {
		return ErrInvalid
	}
	return nil
}

// strict rejects duplicate keys at every depth, aliases, absent/null fields and
// trailing content. Field names are exact, unlike encoding/json's aliases.
func strict(raw []byte, target any, fields string) error {
	if len(raw) == 0 || len(raw) > maxBytes {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var scan func(int) error
	scan = func(depth int) error {
		if depth > 16 {
			return ErrInvalid
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		v, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch v {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := k.(string)
				if !ok || seen[name] {
					return ErrInvalid
				}
				seen[name] = true
				if err := scan(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := scan(depth + 1); err != nil {
					return err
				}
			}
		default:
			return ErrInvalid
		}
		_, err = d.Token()
		return err
	}
	if err := scan(0); err != nil {
		return ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalid
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return ErrInvalid
	}
	names := strings.Fields(fields)
	if len(object) != len(names) {
		return ErrInvalid
	}
	for _, name := range names {
		value, ok := object[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrInvalid
		}
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return ErrInvalid
	}
	return nil
}

const snapshotFields = "protocol instanceID environment application serviceID revision enforcementState migrationEligible currentLicenseJWS pendingLicenseJWS highestLicenseVersion snapshotIssuedAt"

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func decode(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || encode(b) != s {
		return nil, ErrInvalid
	}
	return b, nil
}

// SignSnapshot is a platform-side primitive, never a vendor license issuer.
func SignSnapshot(s PlatformSnapshot, kid string, private ed25519.PrivateKey) (string, error) {
	if err := validateSnapshot(s); err != nil {
		return "", err
	}
	if !identifier(kid) || len(private) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(private.Seed()), private) {
		return "", ErrInvalid
	}
	h, err := json.Marshal(header{"EdDSA", SnapshotType, kid})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	input := encode(h) + "." + encode(p)
	raw := input + "." + encode(ed25519.Sign(private, []byte(input)))
	if len(raw) > maxBytes {
		return "", ErrInvalid
	}
	return raw, nil
}

// VerifySnapshot trusts only configured platform keys and independently verifies
// both embedded licenses against the configured vendor keys.
func VerifySnapshot(raw string, binding Binding, platformKeys, vendorKeys map[string]ed25519.PublicKey) (PlatformSnapshot, error) {
	var s PlatformSnapshot
	if !validBinding(binding) || len(raw) == 0 || len(raw) > maxBytes || len(platformKeys) == 0 || len(vendorKeys) == 0 {
		return s, ErrInvalid
	}
	for _, platformKey := range platformKeys {
		for _, vendorKey := range vendorKeys {
			if bytes.Equal(platformKey, vendorKey) {
				return s, ErrInvalid
			}
		}
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return s, ErrInvalid
	}
	hb, err := decode(parts[0])
	if err != nil {
		return s, err
	}
	var h header
	if err := strict(hb, &h, "alg typ kid"); err != nil {
		return s, err
	}
	key := platformKeys[h.KeyID]
	if h.Algorithm != "EdDSA" || h.Type != SnapshotType || !identifier(h.KeyID) || len(key) != ed25519.PublicKeySize {
		return s, ErrInvalid
	}
	sig, err := decode(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return s, ErrInvalid
	}
	pb, err := decode(parts[1])
	if err != nil {
		return s, err
	}
	if err := strict(pb, &s, snapshotFields); err != nil {
		return s, err
	}
	if err := validateSnapshot(s); err != nil || s.Binding() != binding {
		return PlatformSnapshot{}, ErrInvalid
	}
	current, pending, err := licenses(s, vendorKeys)
	if err != nil {
		return PlatformSnapshot{}, err
	}
	for _, l := range []*core.License{current, pending} {
		if l != nil && (l.InstanceID != binding.InstanceID || l.Environment != binding.Environment || l.Version > s.HighestLicenseVersion) {
			return PlatformSnapshot{}, ErrInvalid
		}
	}
	if current != nil && pending != nil {
		if err := core.CompareVersions(*pending, *current); err != nil || pending.NotBefore <= current.NotBefore {
			return PlatformSnapshot{}, ErrInvalid
		}
	}
	return s, nil
}

func licenses(s PlatformSnapshot, trusted map[string]ed25519.PublicKey) (*core.License, *core.License, error) {
	var current, pending *core.License
	if s.CurrentLicenseJWS != "" {
		l, err := core.Verify(s.CurrentLicenseJWS, trusted)
		if err != nil {
			return nil, nil, fmt.Errorf("current license: %w", err)
		}
		current = &l
	}
	if s.PendingLicenseJWS != "" {
		l, err := core.Verify(s.PendingLicenseJWS, trusted)
		if err != nil {
			return nil, nil, fmt.Errorf("pending license: %w", err)
		}
		pending = &l
	}
	return current, pending, nil
}
