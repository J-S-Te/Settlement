package httpapi

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/j-s-te/settlement/internal/service"
)

// approvalApplicantNames 读取当前可见申请的姓名，优先使用已认证主体的会话资料。
// 参数：ctx 为请求上下文，p 为当前主体，ids 来自已完成租户及数据范围校验的申请记录。
// 返回值：成功核对的姓名；目录失败时调用方仍返回持久化的申请人标识。
func (a *API) approvalApplicantNames(ctx context.Context, p service.Principal, ids []string) map[string]string {
	names := make(map[string]string)
	if len(ids) == 0 {
		return names
	}
	lookup, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	workers := make(chan struct{}, 8)
	pending := make(map[string]bool, len(ids))
	var mu sync.Mutex
	var group sync.WaitGroup
	for _, id := range ids {
		if id == "" || pending[id] {
			continue
		}
		pending[id] = true
		group.Go(func() {
			select {
			case workers <- struct{}{}:
			case <-lookup.Done():
				return
			}
			defer func() { <-workers }()
			if name := a.approvalApplicantName(lookup, p, id); name != "" {
				mu.Lock()
				names[id] = name
				mu.Unlock()
			}
		})
	}
	group.Wait()
	return names
}

// approvalApplicantName 精确核验单个申请人的名字。
// 参数：ctx 为限时上下文，p 为当前会话主体，id 为业务记录已持久化的用户标识。
// 返回值：可信会话或既有租户目录提供的姓名，无法确认时返回空字符串。
func (a *API) approvalApplicantName(ctx context.Context, p service.Principal, id string) string {
	if id == p.UserID {
		if name := strings.TrimSpace(p.DisplayName); name != "" {
			return name
		}
	}
	if a.service.RecognitionDirectory == nil {
		return ""
	}
	people, err := a.service.RecognitionDirectory.RecognitionPeople(ctx, id, "")
	if err != nil || len(people) != 1 || people[0].ID != id {
		return ""
	}
	return strings.TrimSpace(people[0].Name)
}

// decorateApprovalApplicants 按原审批记录的申请人标识补齐名字。
// 参数：ctx 为请求上下文，p 为当前会话主体，items 为已授权的分页结果，field 为申请人标识键。
// 返回值：无；原记录的范围、身份和字段保持原义。
func (a *API) decorateApprovalApplicants(ctx context.Context, p service.Principal, items []map[string]any, field string) {
	ids := make([]string, len(items))
	for index, item := range items {
		ids[index], _ = item[field].(string)
	}
	names := a.approvalApplicantNames(ctx, p, ids)
	for index, item := range items {
		if name := names[ids[index]]; name != "" {
			item["applicant_name"] = name
		}
	}
}
