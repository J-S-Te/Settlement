package service

import (
	"context"
	"errors"
	"testing"
)

// TestAssignmentCannotGrantItself 用途：确认助理及自我扩权在任何外调之前被拒绝。
// 参数：t 为测试上下文。
// 返回值：无，允许自增项目或无权限写入时失败。
func TestAssignmentCannotGrantItself(t *testing.T) {
	s := &Service{}
	p := Principal{TenantID: "t", UserID: "u", Permissions: map[string]bool{"settlement.collaboration.read": true}}
	if err := s.AssignProject(context.Background(), p, ProjectAssignmentInput{ProjectID: "p", UserID: "u", Active: true}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	p.Permissions["settlement.collaboration.manage"] = true
	if err := s.AssignProject(context.Background(), p, ProjectAssignmentInput{ProjectID: "p", UserID: "u", Active: true}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}
