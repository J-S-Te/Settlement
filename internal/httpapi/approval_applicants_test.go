package httpapi

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/settlement/internal/service"
)

type approvalDirectoryFixture struct{}

// RecognitionPeople 返回精确、错配或不可用的测试姓名结果。
// 参数：上下文来自只读响应，userID 为待核验申请人，关键字未使用。
// 返回值：用于演练精确身份匹配及降级的目录结果。
func (approvalDirectoryFixture) RecognitionPeople(_ context.Context, userID, _ string) ([]service.RecognitionPerson, error) {
	switch userID {
	case "historical":
		return []service.RecognitionPerson{{ID: userID, Name: " 历史开票人 "}}, nil
	case "mismatched":
		return []service.RecognitionPerson{{ID: "different", Name: "不能串用姓名"}}, nil
	default:
		return nil, errors.New("人员目录不可用")
	}
}

// TestApprovalApplicantNamesPreserveUnresolvedIDs 验证历史身份精确匹配和目录故障降级。
// 参数：t 为结算 API 读取字段投影的测试上下文。
// 返回值：无；错配资料被采用或原申请人标识丢失时失败。
func TestApprovalApplicantNamesPreserveUnresolvedIDs(t *testing.T) {
	api := API{service: &service.Service{RecognitionDirectory: approvalDirectoryFixture{}}}
	items := []map[string]any{
		{"submitted_by": "historical"},
		{"submitted_by": "mismatched"},
		{"submitted_by": "unavailable"},
	}
	api.decorateApprovalApplicants(context.Background(), service.Principal{UserID: "reviewer"}, items, "submitted_by")
	if items[0]["applicant_name"] != "历史开票人" || items[0]["submitted_by"] != "historical" {
		t.Fatalf("历史申请人姓名未准确返回：%v", items[0])
	}
	for index, id := range []string{"mismatched", "unavailable"} {
		if item := items[index+1]; item["applicant_name"] != nil || item["submitted_by"] != id {
			t.Fatalf("目录异常时未保留原身份：%v", item)
		}
	}
}
