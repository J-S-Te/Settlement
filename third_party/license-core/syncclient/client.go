// Package syncclient 同步已签名的控制快照，不延长商业授权有效期，
// 也不在网络故障时放宽本地门禁。
package syncclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/J-S-Te/license-core/catalog"
	runtime "github.com/J-S-Te/license-core/runtime"
)

const Interval = 30 * time.Second
const maxResponse = 2 * 1024 * 1024

var ErrSync = errors.New("license runtime synchronization failed")

type TokenSource func(context.Context) (string, error)
type LocalRuntime interface {
	ApplySnapshot(context.Context, string, time.Time) error
	ReadState(context.Context) (runtime.State, error)
}
type Options struct {
	BaseURL              string
	ServiceID            string
	CoverageDigest       string
	ImageDigest          string
	RegistrationProtocol int
	CatalogVersion       int
	AllowHTTP            bool
	Client               *http.Client
	Token                TokenSource
	Runtime              LocalRuntime
}
type Client struct {
	options Options
	http    *http.Client
	prefix  string
}
type Snapshot struct {
	RawJWS   string `json:"raw_jws"`
	Digest   string `json:"digest"`
	Revision uint64 `json:"revision"`
}
type acknowledgement struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && strings.ToLower(s) == s
}
func registration(options Options) (int, int, error) {
	protocol, catalogVersion := options.RegistrationProtocol, options.CatalogVersion
	if protocol == 0 && catalogVersion == 0 {
		return catalog.RegistrationProtocolV1, 0, nil
	}
	if protocol == catalog.RegistrationProtocolV1 && catalogVersion == 0 {
		return catalog.RegistrationProtocolV1, 0, nil
	}
	if protocol == catalog.RegistrationProtocolV2 && catalogVersion == catalog.Version {
		return catalog.RegistrationProtocolV2, catalogVersion, nil
	}
	return 0, 0, ErrSync
}

func validOptionalDigest(value string) bool {
	return value == "" || (strings.HasPrefix(value, "sha256:") && validDigest(strings.TrimPrefix(value, "sha256:")))
}

func New(options Options) (*Client, error) {
	u, err := url.Parse(options.BaseURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && options.AllowHTTP)) {
		return nil, ErrSync
	}
	protocol, catalogVersion, err := registration(options)
	if err != nil {
		return nil, ErrSync
	}
	if options.Token == nil || options.Runtime == nil || options.ServiceID == "" || len(options.ServiceID) > 128 || !validOptionalDigest(options.CoverageDigest) || !validOptionalDigest(options.ImageDigest) {
		return nil, ErrSync
	}
	if protocol == catalog.RegistrationProtocolV1 && (options.CoverageDigest == "" || options.ImageDigest == "") {
		return nil, ErrSync
	}
	options.RegistrationProtocol, options.CatalogVersion = protocol, catalogVersion
	for _, r := range options.ServiceID {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:-", r) {
			return nil, ErrSync
		}
	}
	client := http.Client{Timeout: 10 * time.Second}
	if options.Client != nil {
		client = *options.Client
	}
	// 凭据不得随重定向发送，即使目标只是同一主机的另一个路径。
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = 10 * time.Second
	return &Client{options: options, http: &client, prefix: strings.TrimRight(options.BaseURL, "/") + "/api/v1/internal/licenses/runtime/" + url.PathEscape(options.ServiceID)}, nil
}
func (c *Client) request(ctx context.Context, method, action string, input any, output any) error {
	token, err := c.options.Token(ctx)
	if err != nil || token == "" || len(token) > 16*1024 || strings.ContainsAny(token, "\r\n ") {
		return ErrSync
	}
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return ErrSync
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.prefix+"/"+action, body)
	if err != nil {
		return ErrSync
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return ErrSync
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s status %d", ErrSync, action, response.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(b) > maxResponse {
		return ErrSync
	}
	var envelope struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil || envelope.Code != "OK" || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return ErrSync
	}
	if output == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(envelope.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return ErrSync
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ErrSync
	}
	return nil
}

// SyncOnce 仅确认本地运行时已持久化应用的确切签名修订版。
// 确认失败不会回退本地门禁状态。
func (c *Client) SyncOnce(ctx context.Context) error {
	if c == nil {
		return ErrSync
	}
	ready := struct {
		Protocol       int    `json:"protocol"`
		CatalogVersion int    `json:"catalog_version,omitempty"`
		Coverage       string `json:"coverage_digest,omitempty"`
		Image          string `json:"image_digest,omitempty"`
	}{Protocol: c.options.RegistrationProtocol, CatalogVersion: c.options.CatalogVersion, Coverage: c.options.CoverageDigest, Image: c.options.ImageDigest}
	if c.options.RegistrationProtocol == catalog.RegistrationProtocolV1 {
		ready.Protocol = runtime.Protocol
	}
	if err := c.request(ctx, http.MethodPost, "ready", ready, nil); err != nil {
		return err
	}
	var snapshot Snapshot
	if err := c.request(ctx, http.MethodGet, "snapshot", nil, &snapshot); err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(snapshot.RawJWS))
	if snapshot.RawJWS == "" || snapshot.Revision == 0 || snapshot.Digest != hex.EncodeToString(hash[:]) {
		return ErrSync
	}
	if err := c.options.Runtime.ApplySnapshot(ctx, snapshot.RawJWS, time.Now()); err != nil {
		return err
	}
	state, err := c.options.Runtime.ReadState(ctx)
	if err != nil || state.ControlRevision != snapshot.Revision || state.SnapshotJWS != snapshot.RawJWS {
		return ErrSync
	}
	return c.request(ctx, http.MethodPost, "ack", acknowledgement{snapshot.Revision, snapshot.Digest}, nil)
}

// Run 按固定的 30 秒间隔重试。回调只接收错误，
// 不接收响应内容、凭据或签名授权材料。
// 每次业务操作前仍须调用本地 Evaluate。
func (c *Client) Run(ctx context.Context, onError func(error)) error {
	if c == nil || ctx == nil {
		return ErrSync
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.SyncOnce(ctx); err != nil && onError != nil {
			onError(err)
		}
		timer := time.NewTimer(Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
