package authz

import (
	"encoding/json"
	"testing"
)

func TestReviewerCanReadAndApproveWithoutSubmittingInvoices(t *testing.T) {
	var manifest struct {
		Permissions []struct {
			Code string `json:"code"`
		} `json:"permissions"`
		Roles []struct {
			Code        string   `json:"code"`
			Permissions []string `json:"permissions"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(PermissionManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, permission := range manifest.Permissions {
		declared[permission.Code] = true
	}
	if !declared["settlement.invoice.read"] {
		t.Fatal("settlement.invoice.read is not declared")
	}
	for _, role := range manifest.Roles {
		if role.Code != "settlement_reviewer" {
			continue
		}
		granted := map[string]bool{}
		for _, permission := range role.Permissions {
			granted[permission] = true
		}
		if !granted["settlement.invoice.read"] || !granted["settlement.invoice.approve"] || !granted["settlement.invoice.issue"] {
			t.Fatalf("reviewer invoice grants are incomplete: %v", role.Permissions)
		}
		if granted["settlement.invoice.request"] {
			t.Fatal("reviewer must not be able to submit invoice requests")
		}
		return
	}
	t.Fatal("settlement_reviewer role is missing")
}
