package licensecore

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const RecoveryTokenType = "commercial-clock-recovery+jws"

// Recovery is a separately signed, narrowly bound clock recovery credential.
// Successful verification does not consume it or update persisted clock state.
type Recovery struct {
	ProtocolVersion int    `json:"protocol_version"`
	Issuer          string `json:"issuer"`
	ID              string `json:"recovery_id"`
	ProductID       string `json:"product_id"`
	InstanceID      string `json:"instance_id"`
	Environment     string `json:"environment"`
	LicenseID       string `json:"license_id"`
	LicenseVersion  uint64 `json:"license_version"`
	IssuedAt        int64  `json:"issued_at"`
	ExpiresAt       int64  `json:"expires_at"`
	AnchorAt        int64  `json:"anchor_at"`
}

func validateRecoveryFields(r Recovery) error {
	if r.Issuer != Issuer || r.ProtocolVersion != ProtocolVersion || r.ProductID != Product || !identifier(r.ID) || !identifier(r.InstanceID) || !identifier(r.Environment) || !identifier(r.LicenseID) || r.LicenseVersion == 0 || !validTime(r.IssuedAt) || !validTime(r.ExpiresAt) || !validTime(r.AnchorAt) || r.ExpiresAt <= r.IssuedAt || r.AnchorAt < r.IssuedAt || r.AnchorAt > r.ExpiresAt {
		return fmt.Errorf("%w: recovery fields", ErrInvalid)
	}
	return nil
}

// ValidateRecovery checks binding and validity. The caller must provide a trusted
// current time and atomically consume the ID while applying the recovery anchor.
func ValidateRecovery(r Recovery, l License, now time.Time) error {
	if err := validateRecoveryFields(r); err != nil {
		return err
	}
	if err := Validate(l); err != nil {
		return err
	}
	if r.ProductID != l.ProductID || r.InstanceID != l.InstanceID || r.Environment != l.Environment || r.LicenseID != l.ID || r.LicenseVersion != l.Version || now.Unix() < r.IssuedAt || now.Unix() >= r.ExpiresAt {
		return ErrDenied
	}
	return nil
}
func SignRecovery(r Recovery, kid string, key ed25519.PrivateKey) (string, error) {
	if err := validateRecoveryFields(r); err != nil {
		return "", err
	}
	if !identifier(kid) || len(key) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("%w: signing key", ErrInvalid)
	}
	if !bytes.Equal(ed25519.NewKeyFromSeed(key.Seed()), key) {
		return "", fmt.Errorf("%w: signing key", ErrInvalid)
	}
	hb, err := json.Marshal(header{"EdDSA", RecoveryTokenType, kid})
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	input := encoded(hb) + "." + encoded(pb)
	token := input + "." + encoded(ed25519.Sign(key, []byte(input)))
	if len(token) > MaxTokenBytes {
		return "", ErrInvalid
	}
	return token, nil
}
func VerifyRecovery(raw string, trusted map[string]ed25519.PublicKey) (Recovery, error) {
	var r Recovery
	if len(raw) == 0 || len(raw) > MaxTokenBytes {
		return r, ErrInvalid
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return r, ErrInvalid
	}
	hb, err := decode(parts[0])
	if err != nil {
		return r, err
	}
	var h header
	if err = strictJSON(hb, &h); err != nil {
		return r, err
	}
	if h.Algorithm != "EdDSA" || h.Type != RecoveryTokenType || !identifier(h.KeyID) {
		return r, ErrInvalid
	}
	key, ok := trusted[h.KeyID]
	if !ok || len(key) != ed25519.PublicKeySize {
		return r, fmt.Errorf("%w: untrusted key", ErrInvalid)
	}
	pb, err := decode(parts[1])
	if err != nil {
		return r, err
	}
	sig, err := decode(parts[2])
	if err != nil {
		return r, err
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return r, fmt.Errorf("%w: signature", ErrInvalid)
	}
	if err = strictJSON(pb, &r); err != nil {
		return Recovery{}, err
	}
	if err = validateRecoveryFields(r); err != nil {
		return Recovery{}, err
	}
	return r, nil
}
