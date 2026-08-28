package platform

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestValidLogoutClaims(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claims := backchannelLogoutClaims{Subject: "subject", JTI: "jti", Issued: now.Unix() - 10, Expires: now.Unix() + 60, Audience: "settlement", Events: map[string]json.RawMessage{backchannelLogoutEvent: json.RawMessage(`{}`)}}
	if !validLogoutClaims(claims, "settlement", now) {
		t.Fatal("valid logout claims rejected")
	}
	claims.Nonce = "nonce"
	if validLogoutClaims(claims, "settlement", now) {
		t.Fatal("logout token with nonce accepted")
	}
}

func TestValidLogoutTokenType(t *testing.T) {
	header, _ := json.Marshal(map[string]string{"typ": "logout+jwt"})
	raw := base64.RawURLEncoding.EncodeToString(header) + ".payload.signature"
	if !validLogoutTokenType(raw) {
		t.Fatal("logout+jwt type rejected")
	}
	bad, _ := json.Marshal(map[string]string{"typ": "JWT"})
	if validLogoutTokenType(base64.RawURLEncoding.EncodeToString(bad) + ".payload.signature") {
		t.Fatal("ordinary JWT accepted")
	}
}
