package service

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

func TestAgingSummaryOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_DUNNING_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_DUNNING_TEST_MYSQL_DSN is required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`DROP TABLE IF EXISTS settlement_receivable`,
		`CREATE TABLE settlement_receivable (id CHAR(32) PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,open_amount DECIMAL(18,2) NOT NULL,currency CHAR(3) NOT NULL,due_date DATE NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tenant := "tenant-aging"
	if _, err := db.Exec(`INSERT INTO settlement_receivable(id,tenant_id,open_amount,currency,due_date) VALUES
('a1',?,50.00,'CNY',DATE_SUB(CURDATE(),INTERVAL 10 DAY)),
('a2',?,100.00,'CNY',DATE_SUB(CURDATE(),INTERVAL 70 DAY)),
('a3',?,20.00,'USD',DATE_SUB(CURDATE(),INTERVAL 100 DAY)),
('a4',?,0.00,'CNY',DATE_SUB(CURDATE(),INTERVAL 10 DAY))`, tenant, tenant, tenant, tenant); err != nil {
		t.Fatal(err)
	}
	items, err := (&Service{DB: db}).AgingSummary(context.Background(), Principal{TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]AgingBucket{}
	for _, item := range items {
		got[item.Currency+"/"+item.Bucket] = item
	}
	if len(items) != 3 || got["CNY/0-30"].Amount != "50.00" || got["CNY/61-90"].Amount != "100.00" || got["USD/90+"].Amount != "20.00" {
		t.Fatalf("aging=%+v", items)
	}
}
