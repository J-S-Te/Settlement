package service

import (
	"errors"
	"testing"

	"github.com/j-s-te/settlement/internal/projectreference"
)

// TestDecideProjectReferenceUpdate 验证权威项目引用落库规则：
// active 可随权威端下降、版本升高时可受控更换合同、同版本换合同必须澄清冲突，
// 迟到版本不得回退本地快照。
// 参数：t 测试上下文。
// 返回值：无。
func TestDecideProjectReferenceUpdate(t *testing.T) {
	stored := projectReferenceState{Version: 3, ContractID: "contract-a", CustomerID: "customer-a", Name: "项目", Active: true}
	cases := []struct {
		name        string
		ref         projectreference.Reference
		wantState   projectReferenceState
		wantChanged bool
		wantErr     error
	}{
		{
			name:        "same_version_same_contract_deactivates",
			ref:         projectreference.Reference{ProjectID: "project", Version: 3, ContractID: "contract-a", CustomerID: "customer-a", Name: "项目", Active: false},
			wantState:   projectReferenceState{Version: 3, ContractID: "contract-a", CustomerID: "customer-a", Name: "项目", Active: false},
			wantChanged: true,
		},
		{
			name:      "same_version_changed_contract_is_clarified",
			ref:       projectreference.Reference{ProjectID: "project", Version: 3, ContractID: "contract-b", CustomerID: "customer-a", Name: "项目", Active: true},
			wantState: stored,
			wantErr:   ErrProjectReferenceMismatch,
		},
		{
			name:      "same_version_changed_customer_is_clarified",
			ref:       projectreference.Reference{ProjectID: "project", Version: 3, ContractID: "contract-a", CustomerID: "customer-b", Name: "项目", Active: true},
			wantState: stored,
			wantErr:   ErrProjectReferenceMismatch,
		},
		{
			name:        "newer_version_may_change_contract_and_deactivate",
			ref:         projectreference.Reference{ProjectID: "project", Version: 4, ContractID: "contract-b", CustomerID: "customer-b", Name: "新项目", Active: false},
			wantState:   projectReferenceState{Version: 4, ContractID: "contract-b", CustomerID: "customer-b", Name: "新项目", Active: false},
			wantChanged: true,
		},
		{
			name:        "newer_version_same_mapping_reactivates",
			ref:         projectreference.Reference{ProjectID: "project", Version: 4, ContractID: "contract-a", CustomerID: "customer-a", Name: "项目", Active: true},
			wantState:   projectReferenceState{Version: 4, ContractID: "contract-a", CustomerID: "customer-a", Name: "项目", Active: true},
			wantChanged: true,
		},
		{
			name:      "older_version_is_rejected",
			ref:       projectreference.Reference{ProjectID: "project", Version: 2, ContractID: "contract-a", CustomerID: "customer-a", Name: "项目", Active: true},
			wantState: stored,
			wantErr:   ErrConflict,
		},
		{
			name:      "identical_authority_is_noop",
			ref:       projectreference.Reference{ProjectID: "project", Version: 3, ContractID: "contract-a", CustomerID: "customer-a", Name: "项目", Active: true},
			wantState: stored,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, changed, err := decideProjectReferenceUpdate(stored, tc.ref)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v want %v", err, tc.wantErr)
			}
			if next != tc.wantState || changed != tc.wantChanged {
				t.Fatalf("next=%+v changed=%v want %+v/%v", next, changed, tc.wantState, tc.wantChanged)
			}
		})
	}
}

// TestProjectReferenceMismatchKeepsConflictMapping 验证澄清后的映射冲突仍可被
// 通用冲突判断命中，同时不与无效请求混淆。
// 参数：t 测试上下文。
// 返回值：无。
func TestProjectReferenceMismatchKeepsConflictMapping(t *testing.T) {
	if !errors.Is(ErrProjectReferenceMismatch, ErrConflict) {
		t.Fatal("mapping mismatch must remain a conflict for HTTP compatibility")
	}
	if errors.Is(ErrProjectReferenceMismatch, ErrInvalid) {
		t.Fatal("mapping mismatch must not degrade to invalid request")
	}
	if !errors.Is(ErrProjectReferenceInactive, ErrInvalid) {
		t.Fatal("inactive reference must map to invalid request")
	}
}
