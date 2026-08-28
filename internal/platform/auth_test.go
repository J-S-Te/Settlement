package platform

import "testing"

func TestSafeReturnRejectsExternalAndProtocolRelativeURLs(t *testing.T) {
	for _, value := range []string{"https://evil.example/", "//evil.example/", "\\evil", "/settlement/../../admin"} {
		if got := safeReturn(value); got != "/" {
			t.Fatalf("safeReturn(%q)=%q", value, got)
		}
	}
	if got := safeReturn("/receivables?page=2"); got != "/receivables?page=2" {
		t.Fatalf("valid local return changed: %q", got)
	}
}

func TestCanonicalSubjectIDSupportsRollingCompatibility(t *testing.T) {
	for _, input := range [][2]string{{"identity-1", "identity-1"}, {"", "identity-1"}} {
		if got, err := canonicalSubjectID(input[0], input[1]); err != nil || got != "identity-1" {
			t.Fatalf("canonicalSubjectID(%q, %q) = %q, %v", input[0], input[1], got, err)
		}
	}
	if _, err := canonicalSubjectID("subject-1", "identity-1"); err == nil {
		t.Fatal("mismatched subject_id and identity_id were accepted")
	}
}
