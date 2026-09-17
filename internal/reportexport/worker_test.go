package reportexport

import "testing"

func TestNormalizeGatewayMode(t *testing.T) {
	for input, want := range map[string]string{"": "legacy", "legacy": "legacy", "DUAL": "dual", "required": "required", "invalid": "legacy"} {
		if got := NormalizeGatewayMode(input); got != want {
			t.Fatalf("NormalizeGatewayMode(%q)=%q, want %q", input, got, want)
		}
	}
}
