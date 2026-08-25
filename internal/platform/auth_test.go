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
