package reportexport

import "testing"

func TestNormalizeGatewayMode(t *testing.T) {
	for input, want := range map[string]string{"": "legacy", "legacy": "legacy", "DUAL": "dual", "required": "required", "invalid": "legacy"} {
		if got := NormalizeGatewayMode(input); got != want {
			t.Fatalf("NormalizeGatewayMode(%q)=%q, want %q", input, got, want)
		}
	}
}

// safeCell 必须与平台 SEC-B6 escapeCSVFormulaCell 口径一致：公式前缀与前导
// tab/CR 一律加 ' 前缀，普通值原样返回。
func TestSafeCellEscapesFormulaAndControlPrefixes(t *testing.T) {
	tests := []struct{ input, want string }{
		{"=1+1", "'=1+1"},
		{"+SUM(A1)", "'+SUM(A1)"},
		{"-2", "'-2"},
		{"@cmd", "'@cmd"},
		{"\t=cmd|' /C calc'!A0", "'\t=cmd|' /C calc'!A0"},
		{"\r\n=cmd", "'\r\n=cmd"},
		{"\t\t@x", "'\t\t@x"},
		{"普通文本", "普通文本"},
		{" 12.00", " 12.00"},
		{"", ""},
	}
	for _, test := range tests {
		if got := safeCell(test.input); got != test.want {
			t.Fatalf("safeCell(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}
