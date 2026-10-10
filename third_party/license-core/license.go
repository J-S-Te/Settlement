// Package licensecore validates vendor-signed offline commercial licenses.
package licensecore

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

const (
	ProtocolVersion       = 1
	Product               = "unified-business-suite"
	Issuer                = "unified-business-suite-vendor"
	TokenType             = "commercial-license+jws"
	MaxTokenBytes         = 256 * 1024
	maxTimestamp    int64 = 253402300799
)

type Application struct {
	Code      string `json:"code"`
	NotBefore int64  `json:"not_before"`
	ExpiresAt int64  `json:"expires_at"`
	Kind      string `json:"kind"`
}
type License struct {
	ProtocolVersion int           `json:"protocol_version"`
	Issuer          string        `json:"issuer"`
	ID              string        `json:"license_id"`
	Version         uint64        `json:"version"`
	CustomerID      string        `json:"customer_id"`
	ProductID       string        `json:"product_id"`
	Environment     string        `json:"environment"`
	InstanceID      string        `json:"instance_id"`
	IssuedAt        int64         `json:"issued_at"`
	NotBefore       int64         `json:"not_before"`
	Applications    []Application `json:"applications"`
}
type Request struct {
	ProtocolVersion int    `json:"protocol_version"`
	ProductID       string `json:"product_id"`
	Environment     string `json:"environment"`
	InstanceID      string `json:"instance_id"`
	CustomerID      string `json:"customer_id"`
}
type Operation string

const (
	READ_HISTORY      Operation = "READ_HISTORY"
	EXPORT_HISTORY    Operation = "EXPORT_HISTORY"
	MUTATE_BUSINESS   Operation = "MUTATE_BUSINESS"
	ESSENTIAL_SERVICE Operation = "ESSENTIAL_SERVICE"
)

var (
	ErrInvalid  = errors.New("invalid commercial license")
	ErrDenied   = errors.New("commercial license denies operation")
	ErrRollback = errors.New("commercial license rollback rejected")
)
var applications = map[string]bool{"customer_and_opportunity": true, "customer_portal": true, "contract_management": true, "project_management": true, "settlement": true, "data_analysis": true}

func identifier(s string) bool {
	if len(s) == 0 || len(s) > 128 || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validTime(n int64) bool { return n > 0 && n <= maxTimestamp }
func ValidateRequest(r Request) error {
	if r.ProtocolVersion != ProtocolVersion || r.ProductID != Product || !identifier(r.CustomerID) || !identifier(r.Environment) || !identifier(r.InstanceID) {
		return fmt.Errorf("%w: request fields", ErrInvalid)
	}
	return nil
}
func Validate(l License) error {
	if err := ValidateRequest(Request{l.ProtocolVersion, l.ProductID, l.Environment, l.InstanceID, l.CustomerID}); err != nil {
		return err
	}
	if l.Issuer != Issuer || !identifier(l.ID) || l.Version == 0 || !validTime(l.IssuedAt) || !validTime(l.NotBefore) || l.IssuedAt > l.NotBefore || len(l.Applications) == 0 || len(l.Applications) > len(applications) {
		return fmt.Errorf("%w: license fields", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, a := range l.Applications {
		if !applications[a.Code] || seen[a.Code] || !validTime(a.NotBefore) || !validTime(a.ExpiresAt) || a.ExpiresAt <= a.NotBefore || (a.Kind != "FULL" && a.Kind != "TRIAL") {
			return fmt.Errorf("%w: application fields", ErrInvalid)
		}
		seen[a.Code] = true
	}
	return nil
}

type header struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

// strictJSON rejects duplicate keys at every nesting level before decoding.
func strictJSON(data []byte, target any) error {
	if len(data) == 0 || len(data) > MaxTokenBytes {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var scan func() error
	scan = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return ErrInvalid
				}
				seen[s] = true
				if err = scan(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := scan(); err != nil {
					return err
				}
			}
		default:
			return ErrInvalid
		}
		_, err = d.Token()
		return err
	}
	if err := scan(); err != nil {
		return fmt.Errorf("%w: JSON structure", ErrInvalid)
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	// encoding/json normally accepts case-insensitive field aliases. Protocol
	// fields are exact, so reject aliases as well as unknown fields.
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return ErrInvalid
	}
	var allowed string
	switch target.(type) {
	case *header:
		allowed = "alg typ kid"
	case *Request:
		allowed = "protocol_version product_id environment instance_id customer_id"
	case *License:
		allowed = "protocol_version issuer license_id version customer_id product_id environment instance_id issued_at not_before applications"
	case *Recovery:
		allowed = "protocol_version issuer recovery_id product_id instance_id environment license_id license_version issued_at expires_at anchor_at"
	default:
		return ErrInvalid
	}
	for k := range object {
		if !strings.Contains(" "+allowed+" ", " "+k+" ") {
			return ErrInvalid
		}
	}
	if _, ok := target.(*License); ok {
		var apps []map[string]json.RawMessage
		if err := json.Unmarshal(object["applications"], &apps); err != nil {
			return ErrInvalid
		}
		for _, a := range apps {
			for k := range a {
				if !strings.Contains(" code not_before expires_at kind ", " "+k+" ") {
					return ErrInvalid
				}
			}
		}
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("%w: JSON fields", ErrInvalid)
	}
	return nil
}

// ParseRequest applies the same strict JSON policy as license verification.
func ParseRequest(raw []byte) (Request, error) {
	var r Request
	if err := strictJSON(raw, &r); err != nil {
		return r, err
	}
	return r, ValidateRequest(r)
}
func encoded(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func decode(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || encoded(b) != s {
		return nil, ErrInvalid
	}
	return b, nil
}
func Sign(l License, kid string, key ed25519.PrivateKey) (string, error) {
	if err := Validate(l); err != nil {
		return "", err
	}
	if !identifier(kid) || len(key) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("%w: signing key", ErrInvalid)
	}
	// Reject malformed private keys rather than signing unverifiable output.
	if !bytes.Equal(ed25519.NewKeyFromSeed(key.Seed()), key) {
		return "", fmt.Errorf("%w: signing key", ErrInvalid)
	}
	h, err := json.Marshal(header{"EdDSA", TokenType, kid})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	input := encoded(h) + "." + encoded(p)
	token := input + "." + encoded(ed25519.Sign(key, []byte(input)))
	if len(token) > MaxTokenBytes {
		return "", ErrInvalid
	}
	return token, nil
}
func Verify(raw string, trusted map[string]ed25519.PublicKey) (License, error) {
	var l License
	if len(raw) == 0 || len(raw) > MaxTokenBytes {
		return l, ErrInvalid
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return l, ErrInvalid
	}
	hb, err := decode(parts[0])
	if err != nil {
		return l, err
	}
	var h header
	if err = strictJSON(hb, &h); err != nil {
		return l, err
	}
	if h.Algorithm != "EdDSA" || h.Type != TokenType || !identifier(h.KeyID) {
		return l, ErrInvalid
	}
	key, ok := trusted[h.KeyID]
	if !ok || len(key) != ed25519.PublicKeySize {
		return l, fmt.Errorf("%w: untrusted key", ErrInvalid)
	}
	pb, err := decode(parts[1])
	if err != nil {
		return l, err
	}
	sig, err := decode(parts[2])
	if err != nil {
		return l, err
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return l, fmt.Errorf("%w: signature", ErrInvalid)
	}
	if err = strictJSON(pb, &l); err != nil {
		return License{}, err
	}
	if err = Validate(l); err != nil {
		return License{}, err
	}
	return l, nil
}
func Evaluate(l License, app string, operation Operation, now time.Time, instanceID, environment string) error {
	switch operation {
	case READ_HISTORY, EXPORT_HISTORY, MUTATE_BUSINESS, ESSENTIAL_SERVICE:
	default:
		return fmt.Errorf("%w: unknown operation", ErrDenied)
	}
	if err := Validate(l); err != nil {
		return err
	}
	if operation == ESSENTIAL_SERVICE {
		return nil
	}
	if !applications[app] || l.InstanceID != instanceID || l.Environment != environment || now.Unix() < l.NotBefore {
		return ErrDenied
	}
	for _, a := range l.Applications {
		if a.Code == app {
			if now.Unix() < a.NotBefore {
				return ErrDenied
			}
			if operation == MUTATE_BUSINESS && now.Unix() >= a.ExpiresAt {
				return ErrDenied
			}
			return nil
		}
	}
	return ErrDenied
}
func CompareVersions(next, current License) error {
	if err := Validate(next); err != nil {
		return err
	}
	if err := Validate(current); err != nil {
		return err
	}
	if next.CustomerID != current.CustomerID || next.InstanceID != current.InstanceID || next.Environment != current.Environment || next.ProductID != current.ProductID || next.Version <= current.Version {
		return ErrRollback
	}
	return nil
}
