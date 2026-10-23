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
	classificationValue := strings.ToUpper(strings.TrimSpace(classification))
	switch classificationValue {
	case "INVOICE_DOCUMENT", "ELECTRONIC_INVOICE":
		_ = writer.WriteField("purpose", "settlement.invoice.document")
		classificationValue = "CONFIDENTIAL"
	case "REPORT_EXPORT":
		classificationValue = "INTERNAL"
	}
	_ = writer.WriteField("classification", classificationValue)
	contentType := strings.TrimSpace(mediaType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	baseType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", errors.New("invalid upload media type")
	}
	part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": name})}, "Content-Type": {mime.FormatMediaType(baseType, parameters)}})
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

// Release 补偿精确应用及资源绑定，即使 Bind
// 响应丢失。网关保留停用墓碑，使延迟
// 请求无法重新激活已放弃的文件代次。
func (c *Client) Release(ctx context.Context, applicationID, fileID, resourceType, resourceID, bindingType string) error {
	for _, value := range []string{applicationID, fileID, resourceType, resourceID, bindingType} {
		if strings.TrimSpace(value) == "" {
			return errors.New("application, file, resource and binding type are required")
		}
	}
	payload, err := json.Marshal(map[string]string{"application_id": applicationID, "resource_type": resourceType, "resource_id": resourceID, "binding_type": bindingType})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/api/v1/files/"+url.PathEscape(fileID)+"/bindings/release", "", bytes.NewReader(payload), "application/json", nil)
}

// ResolveUpload 仅观察不可变请求。租户和应用
// 来自网关验证后的机器身份，绝非查询参数。
func (c *Client) ResolveUpload(ctx context.Context, requestID string) (fileID, status string, found bool, err error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return "", "", false, errors.New("upload request ID is required")
	}
	var result struct {
		Data struct {
			RequestID string `json:"request_id"`
			FileID    string `json:"file_id"`
			Status    string `json:"status"`
		} `json:"data"`
	}
	err = c.do(ctx, http.MethodGet, "/api/v1/file-upload-requests/"+url.PathEscape(requestID), "", nil, "", &result)
	if err != nil {
		var httpErr *gatewayHTTPError
		if errors.As(err, &httpErr) && httpErr.status == http.StatusNotFound {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	if result.Data.RequestID != requestID {
		return "", "", false, errors.New("file gateway upload lookup identity is invalid")
	}
	if validUploadRequestState(result.Data.FileID, result.Data.Status) {
		return result.Data.FileID, result.Data.Status, true, nil
	}
	return "", "", false, errors.New("file gateway upload lookup state is invalid")
}

// RetireUpload 在放弃业务绑定前隔离不可变上传键。
// 请求缺失时会创建持久化取消墓碑。
func (c *Client) RetireUpload(ctx context.Context, requestID string) (fileID, status string, err error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return "", "", errors.New("upload request ID is required")
	}
	var result struct {
		Data struct {
			RequestID string `json:"request_id"`
			FileID    string `json:"file_id"`
			Status    string `json:"status"`
			Retired   bool   `json:"retired"`
		} `json:"data"`
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/file-upload-requests/"+url.PathEscape(requestID)+"/retire", "", nil, "", &result)
	if err != nil {
		return "", "", err
	}
	if !result.Data.Retired || result.Data.RequestID != requestID || !validUploadRequestState(result.Data.FileID, result.Data.Status) {
		return "", "", errors.New("file gateway upload retirement was not confirmed")
	}
	return result.Data.FileID, result.Data.Status, nil
}

func validUploadRequestState(fileID, status string) bool {
	switch status {
	case "CANCELLED":
		return fileID == ""
	case "WRITING", "READY", "FAILED", "REJECTED":
		return fileID != ""
	default:
		return false
	}
}

type gatewayHTTPError struct{ status int }

func (e *gatewayHTTPError) Error() string {
	return fmt.Sprintf("file gateway returned HTTP %d", e.status)
}

// Download 在结算 API 已确认调用方业务资源访问权限后，
// 读取 READY 文件。有界缓冲区防止文件
// 网关响应耗尽 API 内存。
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
		return &gatewayHTTPError{status: resp.StatusCode}
	}
	if target != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
			return err
		}
	}
	return nil
}
