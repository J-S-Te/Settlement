package projectreference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

type Reference struct {
	TenantID   string `json:"tenant_id"`
	ProjectID  string `json:"project_id"`
	ContractID string `json:"contract_id"`
	CustomerID string `json:"customer_id"`
	Name       string `json:"name"`
	Version    uint64 `json:"version"`
	Active     bool   `json:"active"`
}
type Client struct {
	baseURL string
	client  *http.Client
}

// New 构造使用专用只读机器scope的项目引用客户端。
// 参数：baseURL 为项目服务，clientID/clientSecret 为机器凭据，tokenURL 为令牌端点，timeout 为超时。
// 返回值：客户端；配置不完整或URL不合法时返回错误。
func New(baseURL, clientID, clientSecret, tokenURL string, timeout time.Duration) (*Client, error) {
	for _, raw := range []string{baseURL, tokenURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("project reference requires valid HTTP(S) endpoints")
		}
	}
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" || timeout <= 0 {
		return nil, fmt.Errorf("project reference OAuth configuration is incomplete")
	}
	transport := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, transport)
	cfg := clientcredentials.Config{ClientID: clientID, ClientSecret: clientSecret, TokenURL: tokenURL, Scopes: []string{"settlement.project_reference.read"}}
	httpClient := cfg.Client(ctx)
	httpClient.Timeout = timeout
	httpClient.CheckRedirect = transport.CheckRedirect
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), client: httpClient}, nil
}

// Resolve 获取并复核权威项目、合同及客户引用。
// 参数：ctx 控制HTTP请求，tenant 为本地可信租户，projectID 为精确项目标识。
// 返回值：权威引用；跨租户、错项目、缺失字段或远端失败返回错误。
func (c *Client) Resolve(ctx context.Context, tenant, projectID string) (Reference, error) {
	if strings.TrimSpace(tenant) == "" || projectID == "" || projectID != strings.TrimSpace(projectID) || len(projectID) > 128 || strings.ContainsAny(projectID, "/\\?#") {
		return Reference{}, fmt.Errorf("invalid project reference identity")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/internal/v1/settlement/project-references/"+url.PathEscape(projectID), nil)
	if err != nil {
		return Reference{}, err
	}
	response, err := c.client.Do(req)
	if err != nil {
		return Reference{}, fmt.Errorf("project reference unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Reference{}, fmt.Errorf("project reference returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(body) > 65536 {
		return Reference{}, fmt.Errorf("invalid project reference response")
	}
	var result struct {
		Code string    `json:"code"`
		Data Reference `json:"data"`
	}
	if err = json.Unmarshal(body, &result); err != nil || result.Code != "OK" {
		return Reference{}, fmt.Errorf("invalid project reference response")
	}
	ref := result.Data
	if ref.TenantID != tenant || ref.ProjectID != projectID || ref.ContractID == "" || ref.CustomerID == "" || ref.Version == 0 {
		return Reference{}, fmt.Errorf("project reference identity mismatch or incomplete authority")
	}
	return ref, nil
}
