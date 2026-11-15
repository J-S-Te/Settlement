package httpapi

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/service"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestInvoiceBusinessFeedbackHTTP 用途：验证真实服务校验带安全行号字段且保留原HTTP错误码。
// 参数：t 为无数据库依赖的HTTP测试上下文。
// 返回值：无；缺少字段定位、公开原因或错误码漂移时失败。
func TestInvoiceBusinessFeedbackHTTP(t *testing.T) {
	in := service.InvoiceRequestInput{InvoiceType: "ELECTRONIC_NORMAL", Items: []service.InvoiceItemInput{{Quantity: "1", UnitPriceExclTax: "10",
		AmountExclTax: "10", TaxRate: "0.06", TaxAmount: "0.61", AmountInclTax: "10.61"}},
		Allocations: []service.InvoiceAllocationInput{{ReceivableID: "receivable", ReservedAmount: "10.61"}}}
	_, err := (&service.Service{}).CreateInvoiceRequest(t.Context(), service.Principal{}, "key", in)
	if !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("未命中明细拒绝: %v", err)
	}
	w := httptest.NewRecorder()
	respondError(w, fmt.Errorf("wrapper: %w", err))
	var body struct {
		Code, Message string
		Details       struct {
			ReasonCode string `json:"reason_code"`
			Issues     []struct {
				Field   string
				Row     int
				Message string
			}
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 400 || body.Code != "SETTLEMENT_INVALID_REQUEST" ||
		!strings.Contains(body.Message, "第 1 行税额应为 0.60") || len(body.Details.Issues) != 1 ||
		body.Details.Issues[0].Field != "items[0].tax_amount" || body.Details.Issues[0].Row != 1 {
		t.Fatalf("业务字段回执不完整: %s", w.Body.String())
	}
}

// TestApprovalBusinessFeedbackHTTP 用途：验证现有审批独立性及税控冲突的公开拒绝原因。
// 参数：t 为脚本驱动服务与真实HTTP映射的测试上下文。
// 返回值：无；业务原因丢失、状态码变化或内部文本外泄时失败。
func TestApprovalBusinessFeedbackHTTP(t *testing.T) {
	for _, tc := range []struct {
		applicant, ordinary, user, want string
		status                          int
	}{
		{"applicant", "finance", "applicant", "申请人不能加签", 403},
		{"applicant", "", "director", "缺少普通审核人", 409},
		{"applicant", "director", "director", "普通审核人与总监加签必须不同人", 403},
	} {
		t.Run(tc.want, func(t *testing.T) {
			db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"},
				scriptdb.Step{Kind: "query", SQL: "status='SPECIAL_REVIEW'", Rows: [][]driver.Value{{tc.applicant, tc.ordinary, "snapshot"}}},
				scriptdb.Step{Kind: "query", SQL: "SELECT migration_state",
					Rows: [][]driver.Value{{"RELEASED"}}},
				scriptdb.Step{Kind: "query", SQL: "SELECT migration_state",
					Rows: [][]driver.Value{{"RELEASED"}}},
				scriptdb.Step{Kind: "rollback"})
			_, err := (&service.Service{DB: db}).ReviewSpecialInvoice(t.Context(), service.Principal{
				TenantID: "tenant", UserID: tc.user, Permissions: map[string]bool{"settlement.invoice.special_approve": true},
			}, "request", 1, true, "")
			w := httptest.NewRecorder()
			respondError(w, err)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("审批原因没有到达HTTP: %s", w.Body.String())
			}
		})
	}
}

// TestBusinessFeedbackDoesNotPublishArbitraryCause 用途：验证未标记的内部错误不会借中文文本泄漏。
// 参数：t 为HTTP响应测试上下文。
// 返回值：无；内部异常或伪造业务文案进入响应时失败。
func TestBusinessFeedbackDoesNotPublishArbitraryCause(t *testing.T) {
	for _, err := range []error{fmt.Errorf("%w: 内部数据库密码已变化", service.ErrConflict), errors.New("内部数据库密码已变化")} {
		w := httptest.NewRecorder()
		respondError(w, err)
		if strings.Contains(w.Body.String(), "密码") {
			t.Fatal(w.Body.String())
		}
	}
}

// TestProviderConflictFeedbackHTTP 用途：验证税控冲突禁止重开票的原因不会退化成刷新重试。
// 参数：t 为脚本SQL与真实HTTP映射测试上下文。
// 返回值：无；HTTP状态、对账指引或禁止重复开具说明缺失时失败。
func TestProviderConflictFeedbackHTTP(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "SELECT submitted_by", Rows: [][]driver.Value{{"applicant"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT required_review", Rows: [][]driver.Value{{false}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT COUNT(*) FROM settlement_tax_result_inbox", Rows: [][]driver.Value{{int64(1)}}},
		scriptdb.Step{Kind: "rollback"})
	_, err := (&service.Service{DB: db}).ApproveInvoiceRequest(t.Context(),
		service.Principal{TenantID: "tenant", UserID: "finance"}, "request", 1)
	w := httptest.NewRecorder()
	respondError(w, err)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "禁止重新开票") ||
		!strings.Contains(w.Body.String(), "核对税控结果") {
		t.Fatal(w.Body.String())
	}
}

// TestSpecialReviewHidesFeedbackOutsideContractScope 验证范围外主体不能获知对象审核事实。
// 参数：t 为脚本SQL与真实HTTP映射测试上下文，不连接业务库。
// 返回值：无；不可见回执泄漏申请人或普通审核状态时失败。
func TestSpecialReviewHidesFeedbackOutsideContractScope(t *testing.T) {
	for _, facts := range [][2]string{
		{"outside-director", "finance"}, {"applicant", ""}, {"applicant", "outside-director"},
	} {
		db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"},
			scriptdb.Step{Kind: "query", SQL: "status='SPECIAL_REVIEW'",
				Rows: [][]driver.Value{{facts[0], facts[1], "snapshot"}}},
			scriptdb.Step{Kind: "query", SQL: "SELECT migration_state",
				Rows: [][]driver.Value{{"RELEASED"}}},
			scriptdb.Step{Kind: "query", SQL: "SELECT migration_state",
				Rows: [][]driver.Value{{"RELEASED"}}},
			scriptdb.Step{Kind: "rollback"})
		_, err := (&service.Service{DB: db}).ReviewSpecialInvoice(t.Context(), service.Principal{
			TenantID: "tenant", UserID: "outside-director", CatalogVersion: "scope-test",
			Permissions: map[string]bool{"settlement.invoice.special_approve": true},
			Roles:       []string{"settlement_finance_manager"},
			DataScopes: []service.DataScope{{RoleCode: "settlement_finance_manager",
				ScopeType: "ORG_UNIT", ScopeID: "outside-org"}},
		}, "request", 1, true, "")
		response := httptest.NewRecorder()
		respondError(response, err)
		if !errors.Is(err, service.ErrNotFound) || response.Code != 404 ||
			strings.Contains(response.Body.String(), "reason_code") ||
			strings.Contains(response.Body.String(), "审核人") ||
			strings.Contains(response.Body.String(), "申请人") {
			t.Errorf("范围外回执泄漏对象事实：%v，%s", err, response.Body.String())
		}
	}
}
