package service

import (
	"context"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// TestAgingSummaryOnMySQL 用途：在新建隔离schema验证默认五档账龄及按币种汇总。
// 参数：t为测试上下文，仅接受SETTLEMENT_TEST_DSN的专用测试库。
// 返回值：无，实际账龄分档或余额异常时测试失败。
func TestAgingSummaryOnMySQL(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	tenant := "tenant-aging"
	invoice := seedArchiveInvoice(t, db, tenant)
	var snapshot string
	if err := db.QueryRow(`SELECT ir.contract_snapshot_id FROM settlement_invoice_request ir JOIN settlement_tax_invoice ti ON ti.invoice_request_id=ir.id AND ti.tenant_id=ir.tenant_id WHERE ti.id=? AND ti.tenant_id=?`, invoice, tenant).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		days             int
		amount, currency string
	}{{10, "50.00", "CNY"}, {70, "100.00", "CNY"}, {100, "20.00", "USD"}, {200, "30.00", "USD"}, {10, "0.00", "CNY"}} {
		id := newID()
		if _, err := db.Exec(`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,due_date,original_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,?,?,?,DATE_SUB(CURDATE(),INTERVAL ? DAY),100,?,?, 'CONFIRMED','UNPAID','NOT_INVOICED',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, "AR-"+id[:12], snapshot, entry.days, entry.amount, entry.currency); err != nil {
			t.Fatal(err)
		}
	}
	items, err := (&Service{DB: db}).AgingSummary(context.Background(), Principal{TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]AgingBucket{}
	for _, item := range items {
		got[item.Currency+"/"+item.Bucket] = item
	}
	if len(items) != 4 || got["CNY/0-30"].Amount != "50.00" || got["CNY/61-90"].Amount != "100.00" || got["USD/91-180"].Amount != "20.00" || got["USD/180+"].Amount != "30.00" {
		t.Fatalf("aging=%+v", items)
	}
}
