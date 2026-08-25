package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PersonnelDirectory 查询基础平台中当前 Settlement 应用下的有效催收人员。
// 查询结果只包含用户 ID、显示名和角色，不返回账号、邮箱或手机号。
type PersonnelDirectory interface {
	ListCollectors(ctx context.Context, keyword string, page, pageSize int) ([]Personnel, int64, error)
	ContainsCollector(ctx context.Context, userID string) (bool, error)
}

type Personnel struct {
	UserID      string   `json:"user_id"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
}

// HTTPPersonnelDirectory 使用平台机器客户端凭据访问人员目录。
type HTTPPersonnelDirectory struct {
	baseURL, clientID, clientSecret string
	client                          *http.Client
}

func NewPersonnelDirectory(baseURL, clientID, clientSecret string, timeout time.Duration) PersonnelDirectory {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPPersonnelDirectory{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), clientID: strings.TrimSpace(clientID),
		clientSecret: clientSecret, client: &http.Client{Timeout: timeout},
	}
}

func (c *HTTPPersonnelDirectory) ListCollectors(ctx context.Context, keyword string, page, pageSize int) ([]Personnel, int64, error) {
	if c == nil || c.client == nil {
		return nil, 0, fmt.Errorf("personnel directory is not configured")
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 50
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	query := url.Values{
		"role_code": {"settlement_collector"},
		"page":      {strconv.Itoa(page)},
		"page_size": {strconv.Itoa(pageSize)},
	}
	if keyword = strings.TrimSpace(keyword); keyword != "" {
		query.Set("keyword", keyword)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/internal/owner-directory?"+query.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("query personnel directory: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil, 0, fmt.Errorf("personnel directory returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data struct {
			Items []Personnel `json:"items"`
			Total int64       `json:"total"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		return nil, 0, fmt.Errorf("decode personnel directory: %w", err)
	}
	for index := range envelope.Data.Items {
		item := &envelope.Data.Items[index]
		item.UserID = strings.TrimSpace(item.UserID)
		item.DisplayName = strings.TrimSpace(item.DisplayName)
		if item.UserID == "" || item.DisplayName == "" {
			return nil, 0, fmt.Errorf("personnel directory returned incomplete user")
		}
		item.Roles = []string{"settlement_collector"}
	}
	sort.Slice(envelope.Data.Items, func(i, j int) bool {
		if envelope.Data.Items[i].DisplayName == envelope.Data.Items[j].DisplayName {
			return envelope.Data.Items[i].UserID < envelope.Data.Items[j].UserID
		}
		return envelope.Data.Items[i].DisplayName < envelope.Data.Items[j].DisplayName
	})
	return envelope.Data.Items, envelope.Data.Total, nil
}

func (c *HTTPPersonnelDirectory) ContainsCollector(ctx context.Context, userID string) (bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, fmt.Errorf("personnel user ID is required")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return false, err
	}
	query := url.Values{"role_code": {"settlement_collector"}, "user_id": {userID}, "page": {"1"}, "page_size": {"1"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/internal/owner-directory?"+query.Encode(), nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.client.Do(request)
	if err != nil {
		return false, fmt.Errorf("query personnel directory: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return false, fmt.Errorf("personnel directory returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data struct {
			Items []struct {
				UserID string `json:"user_id"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		return false, fmt.Errorf("decode personnel directory: %w", err)
	}
	for _, item := range envelope.Data.Items {
		if strings.TrimSpace(item.UserID) == userID {
			return true, nil
		}
	}
	return false, nil
}

func (c *HTTPPersonnelDirectory) accessToken(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"owner_directory.read"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.SetBasicAuth(c.clientID, c.clientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("request personnel directory token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return "", fmt.Errorf("personnel directory token returned HTTP %d", response.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token); err != nil {
		return "", fmt.Errorf("decode personnel directory token: %w", err)
	}
	if token.AccessToken == "" || !strings.EqualFold(token.TokenType, "bearer") || !hasScope(token.Scope, "owner_directory.read") {
		return "", fmt.Errorf("personnel directory token missing owner_directory.read scope")
	}
	return token.AccessToken, nil
}
