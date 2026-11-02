package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/settlement/internal/service"
)

// TestRespondErrorClarifiesProjectReferenceConflicts 验证项目引用的两类权威冲突
// 返回可识别的HTTP码，而不是退化成通用“记录已变化”或内部错误。
// 参数：t 测试上下文。
// 返回值：无。
func TestRespondErrorClarifiesProjectReferenceConflicts(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"same_version_contract_change", service.ErrProjectReferenceMismatch, http.StatusConflict, "SETTLEMENT_PROJECT_REFERENCE_MISMATCH"},
		{"inactive_authority", service.ErrProjectReferenceInactive, http.StatusBadRequest, "SETTLEMENT_PROJECT_REFERENCE_INACTIVE"},
		{"wrapped_mismatch", errors.Join(errors.New("sync"), service.ErrProjectReferenceMismatch), http.StatusConflict, "SETTLEMENT_PROJECT_REFERENCE_MISMATCH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			respondError(recorder, tc.err)
			if recorder.Code != tc.status {
				t.Fatalf("status=%d want %d", recorder.Code, tc.status)
			}
			var body map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body["code"] != tc.code || body["message"] == "" {
				t.Fatalf("body=%+v want code %s", body, tc.code)
			}
		})
	}
}
