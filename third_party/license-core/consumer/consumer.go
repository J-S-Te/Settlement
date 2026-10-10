// Package consumer is the standard-library runtime bootstrap shared by business
// services. Deployment configuration supplies only platform trust; vendor trust
// is compiled into the reviewed release, never imported from customer settings.
package consumer

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	core "github.com/J-S-Te/license-core"
	runtime "github.com/J-S-Te/license-core/runtime"
	"github.com/J-S-Te/license-core/syncclient"
)

var ErrConfiguration = errors.New("invalid commercial runtime configuration")

const vendorPEM = "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAaKc7dKpa6IPeM3+dRf9fai2jD8LvwKssmm68Up++un8=\n-----END PUBLIC KEY-----"

type Gate struct {
	runtime *runtime.FileRuntime
	sync    *syncclient.Client
}

func (g *Gate) Check(ctx context.Context, op core.Operation) error {
	if g == nil {
		return ErrConfiguration
	}
	if g.runtime == nil {
		switch op {
		case core.READ_HISTORY, core.EXPORT_HISTORY, core.MUTATE_BUSINESS, core.ESSENTIAL_SERVICE:
			return nil
		default:
			return core.ErrDenied
		}
	}
	return g.runtime.Evaluate(ctx, op, time.Now().UTC())
}
func (g *Gate) Run(ctx context.Context, onError func(error)) error {
	if g == nil || ctx == nil {
		return ErrConfiguration
	}
	if g.sync == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return g.sync.Run(ctx, onError)
}
func publicKey(raw []byte) (ed25519.PublicKey, error) {
	b, rest := pem.Decode(raw)
	if b == nil || b.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, ErrConfiguration
	}
	k, err := x509.ParsePKIXPublicKey(b.Bytes)
	key, ok := k.(ed25519.PublicKey)
	if err != nil || !ok || len(key) != ed25519.PublicKeySize {
		return nil, ErrConfiguration
	}
	return key, nil
}

// FromEnvironment is called once at startup. No fallback is permitted when
// controlled licensing is enabled but trust, binding or credentials are absent.
func FromEnvironment(application string) (*Gate, error) { return FromLookup(application, os.Getenv) }

// Compatibility is only for never-enrolled components. The fixed durable mount
// prevents a missing env file after restart from silently disabling enforcement.
func compatibilityAllowed(get func(string) string, stateDirectory string) error {
	for _, field := range []string{"INSTANCE_ID", "ENVIRONMENT", "SERVICE_ID", "STATE_PATH", "PLATFORM_PUBLIC_KEY_PATH", "PLATFORM_BASE_URL", "CLIENT_ID", "CLIENT_SECRET", "COVERAGE_DIGEST", "IMAGE_DIGEST"} {
		if get("COMMERCIAL_LICENSE_"+field) != "" {
			return ErrConfiguration
		}
	}
	entries, err := os.ReadDir(stateDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || len(entries) != 0 {
		return ErrConfiguration
	}
	return nil
}

func FromLookup(application string, get func(string) string) (*Gate, error) {
	if get == nil {
		return nil, ErrConfiguration
	}
	v := get("COMMERCIAL_LICENSE_ENABLED")
	if v == "" || v == "false" {
		if err := compatibilityAllowed(get, "/var/lib/commercial-license"); err != nil {
			return nil, err
		}
		return &Gate{}, nil
	}
	if v != "true" {
		return nil, ErrConfiguration
	}
	read := func(name string) string { return get("COMMERCIAL_LICENSE_" + name) }
	binding := runtime.Binding{InstanceID: read("INSTANCE_ID"), Environment: read("ENVIRONMENT"), Application: application, ServiceID: read("SERVICE_ID")}
	f, err := os.Open(read("PLATFORM_PUBLIC_KEY_PATH"))
	if err != nil {
		return nil, ErrConfiguration
	}
	b, readErr := io.ReadAll(io.LimitReader(f, 8193))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(b) > 8192 {
		return nil, ErrConfiguration
	}
	pk, err := publicKey(b)
	if err != nil {
		return nil, err
	}
	vk, err := publicKey([]byte(vendorPEM))
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(pk)
	rt, err := runtime.NewFileRuntime(read("STATE_PATH"), binding, map[string]ed25519.PublicKey{"platform-" + hex.EncodeToString(hash[:16]): pk}, map[string]ed25519.PublicKey{"vendor-v1": vk})
	if err != nil {
		return nil, ErrConfiguration
	}
	allowHTTP := false
	if raw := read("ALLOW_HTTP"); raw != "" {
		allowHTTP, err = strconv.ParseBool(raw)
		if err != nil {
			return nil, ErrConfiguration
		}
	}
	u, err := url.Parse(read("PLATFORM_BASE_URL"))
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return nil, ErrConfiguration
	}
	id, secret := read("CLIENT_ID"), read("CLIENT_SECRET")
	if id == "" || secret == "" || strings.ContainsAny(id+secret, "\r\n") {
		return nil, ErrConfiguration
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("credential redirect denied") }}
	token := func(ctx context.Context) (string, error) {
		form := url.Values{"grant_type": {"client_credentials"}, "scope": {"license.runtime"}}
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(u.String(), "/")+"/oauth2/token", strings.NewReader(form.Encode()))
		if err != nil {
			return "", syncclient.ErrSync
		}
		req.SetBasicAuth(id, secret)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			return "", syncclient.ErrSync
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
		if err != nil || len(raw) > 65536 || resp.StatusCode != 200 {
			return "", syncclient.ErrSync
		}
		var out struct {
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
		}
		if json.Unmarshal(raw, &out) != nil || !strings.EqualFold(out.TokenType, "Bearer") || out.AccessToken == "" {
			return "", syncclient.ErrSync
		}
		return out.AccessToken, nil
	}
	sc, err := syncclient.New(syncclient.Options{BaseURL: u.String(), ServiceID: binding.ServiceID, CoverageDigest: read("COVERAGE_DIGEST"), ImageDigest: read("IMAGE_DIGEST"), AllowHTTP: allowHTTP, Client: client, Token: token, Runtime: rt})
	if err != nil {
		return nil, ErrConfiguration
	}
	return &Gate{runtime: rt, sync: sc}, nil
}
