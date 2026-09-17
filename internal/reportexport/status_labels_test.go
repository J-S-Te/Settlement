package reportexport

import "testing"

func TestExportStatusLabelsAreChinese(t *testing.T) {
	for input, expected := range map[string]string{"UNPAID": "未回款", "PARTIALLY_SETTLED": "部分回款", "SETTLED": "已结清"} {
		if actual := collectionStatusLabel(input); actual != expected {
			t.Fatalf("collection status %s: got %q", input, actual)
		}
	}
	for input, expected := range map[string]string{"NOT_INVOICED": "未开票", "PARTIALLY_INVOICED": "部分开票", "FULLY_INVOICED": "已全部开票"} {
		if actual := invoiceStatusLabel(input); actual != expected {
			t.Fatalf("invoice status %s: got %q", input, actual)
		}
	}
}
