// Package filegatewayclient 提供结算服务访问基础平台文件网关的本地 HTTP 适配器。
package filegatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

const maxUploadBytes int64 = 20 << 20

// TokenSource 返回结算服务已获取的文件网关 Bearer 令牌。
type TokenSource func(context.Context) (string, error)

// Client 是结算服务独立的文件网关访问适配器，不共享其他仓库实现。
type Client struct {
	baseURL    string
	httpClient *http.Client
	token      TokenSource
}

// New 创建客户端并检查平台 API 地址和令牌来源。
func New(baseURL string, httpClient *http.Client, token TokenSource) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("file gateway base URL must be an HTTP(S) origin")
	}
	if token == nil {
		return nil, errors.New("file gateway token source is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{baseURL: baseURL, httpClient: httpClient, token: token}, nil
}

// Upload 上传结算附件并返回文件 ID；requestID 用于完整请求哈希幂等。
func (c *Client) Upload(ctx context.Context, requestID, applicationID, classification, name, mediaType string, content io.Reader) (string, error) {
	if strings.TrimSpace(requestID) == "" || strings.TrimSpace(applicationID) == "" || strings.TrimSpace(name) == "" || content == nil {
		return "", errors.New("request ID, application ID, file name and content are required")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("application_id", applicationID)
	_ = writer.WriteField("classification", classification)
	contentType := strings.TrimSpace(mediaType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="` + strings.ReplaceAll(name, `"`, "") + `"`}, "Content-Type": {mime.FormatMediaType(contentType, nil)}})
	if err != nil {
		return "", err
	}
	if _, err = io.Copy(part, io.LimitReader(content, maxUploadBytes+1)); err != nil {
		return "", err
	}
	if int64(body.Len()) > maxUploadBytes+(1<<20) {
		return "", errors.New("upload exceeds 20 MiB")
	}
	if err = writer.Close(); err != nil {
		return "", err
	}
	var result struct {
		Data struct {
			FileID string `json:"file_id"`
		} `json:"data"`
	}
	if err = c.do(ctx, http.MethodPost, "/api/v1/files", requestID, &body, writer.FormDataContentType(), &result); err != nil {
		return "", err
	}
	if result.Data.FileID == "" {
		return "", errors.New("file gateway response missing file_id")
	}
	return result.Data.FileID, nil
}

// Bind 将已就绪文件绑定到结算资源，resourceID 必须由结算业务层校验归属。
func (c *Client) Bind(ctx context.Context, applicationID, fileID, resourceType, resourceID, bindingType, displayName string) error {
	payload, err := json.Marshal(map[string]any{"application_id": applicationID, "resource_type": resourceType, "resource_id": resourceID, "binding_type": bindingType, "display_name": displayName})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/api/v1/files/"+url.PathEscape(fileID)+"/bindings", "", bytes.NewReader(payload), "application/json", nil)
}

// Download reads a READY file after the settlement API has already verified
// the caller's business-resource access. The bounded buffer prevents a file
// gateway response from exhausting API memory.
func (c *Client) Download(ctx context.Context, fileID string) ([]byte, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return nil, errors.New("file ID is required")
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get file gateway token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/files/"+url.PathEscape(fileID)+"/content", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file gateway object: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("file gateway returned HTTP %d", resp.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read file gateway response: %w", err)
	}
	if int64(len(content)) > maxUploadBytes {
		return nil, errors.New("file gateway response exceeds 20 MiB")
	}
	return content, nil
}

func (c *Client) do(ctx context.Context, method, path, requestID string, body io.Reader, contentType string, target any) error {
	token, err := c.token(ctx)
	if err != nil {
		return fmt.Errorf("get file gateway token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if requestID = strings.TrimSpace(requestID); requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
		req.Header.Set("Idempotency-Key", requestID)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("file gateway returned HTTP %d", resp.StatusCode)
	}
	if target != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
			return err
		}
	}
	return nil
}
