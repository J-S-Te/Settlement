package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/j-s-te/settlement/internal/service"
)

// RecognitionPeople 精确查询受控在职人员与组织，保持目录机器凭据的当前租户边界。
// 参数：ctx为超时上下文，userID可精确定位，keyword用于有界候选搜索。
// 返回值：最多50名最小人员组织资料；远端失败或超限拒绝。
func (c *HTTPPersonnelDirectory) RecognitionPeople(ctx context.Context, userID, keyword string) ([]service.RecognitionPerson, error) {
	if c == nil || len(userID) > 128 || len(keyword) > 100 {
		return nil, fmt.Errorf("personnel directory unavailable")
	}
	token, e := c.accessToken(ctx)
	if e != nil {
		return nil, e
	}
	query := url.Values{"page": {"1"}, "page_size": {"50"}}
	if userID != "" {
		query.Set("user_id", userID)
		query.Set("page_size", "2")
	}
	if keyword != "" {
		query.Set("keyword", keyword)
	}
	req, e := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v1/internal/owner-directory?"+query.Encode(), nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, e := c.client.Do(req)
	if e != nil {
		return nil, e
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("personnel directory unavailable")
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if e != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("personnel directory limit")
	}
	var envelope struct {
		Data struct {
			Total int                         `json:"total"`
			Items []service.RecognitionPerson `json:"items"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Data.Items == nil || len(envelope.Data.Items) > 50 {
		return nil, fmt.Errorf("invalid personnel directory")
	}
	if userID != "" && (envelope.Data.Total != 1 || len(envelope.Data.Items) != 1 || envelope.Data.Items[0].ID != userID) {
		return nil, fmt.Errorf("personnel identity unavailable")
	}
	for _, person := range envelope.Data.Items {
		if strings.TrimSpace(person.ID) == "" || person.Name == "" {
			return nil, fmt.Errorf("invalid personnel identity")
		}
	}
	return envelope.Data.Items, nil
}
