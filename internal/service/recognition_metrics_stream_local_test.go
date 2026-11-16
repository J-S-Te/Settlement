package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLocalSettlementMetricStreamHistory 验证真实MySQL超万行且超旧8MiB指标完整流出。
// 参数：t 为测试上下文，仅显式回环local_fix_settlement私有DSN被接受。
// 返回值：无；源租户、计数、摘要、块边界或金额口径不符时失败。
func TestLocalSettlementMetricStreamHistory(t *testing.T) {
	path := os.Getenv("LOCAL_METRIC_STREAM_DSN_FILE")
	if path == "" {
		t.Skip("explicit local stream DSN file required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private DSN file required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read DSN failed")
	}
	cfg, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal("invalid DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || host != "127.0.0.1" || cfg.DBName != "local_fix_settlement" || !cfg.ParseTime {
		t.Fatal("explicit local_fix_settlement required")
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("isolated database unavailable")
	}
	defer db.Close()
	tenant := newID()
	now := time.Now().UTC()
	defer db.Exec("DELETE FROM settlement_recognition WHERE tenant_id=?", tenant)
	for first := 0; first < 10001; first += 200 {
		last := first + 200
		if last > 10001 {
			last = 10001
		}
		marks := []string{}
		args := []any{}
		for i := first; i < last; i++ {
			id := newID()
			record := Recognition{RecognitionInput: RecognitionInput{Kind: "INCOME", OrganizationID: "org", IdentityID: "person", ProjectID: "project", Currency: "CNY", RecognitionDate: "2026-11-01", SourceSystem: "manual"}, ID: id, TenantID: tenant, OrganizationName: strings.Repeat("a", 256), IdentityName: strings.Repeat("b", 256), ProjectName: strings.Repeat("c", 256), NetMinor: int64(i), GrossMinor: int64(i), Status: "CONFIRMED", CreatedAt: now, UpdatedAt: now}
			payload, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			marks = append(marks, "(?,?,?,?,?,?,?,?,?,?)")
			args = append(args, id, tenant, "org", "person", "CONFIRMED", "", "", payload, now, now)
		}
		query := "INSERT INTO settlement_recognition(id,tenant_id,organization_id,identity_id,status,source_claim_id,original_id,payload,created_at,updated_at) VALUES " + strings.Join(marks, ",")
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := (&Service{DB: db}).StreamDashboardMetrics(context.Background(), tenant, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() <= 8<<20 {
		t.Fatal("fixture does not exceed legacy byte limit")
	}
	digest := sha256.New()
	count, sequence := 0, 0
	ended := false
	var sum int64
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'}) {
		if len(line) > 1<<20 {
			t.Fatal("frame bound")
		}
		var frame struct {
			Type     string
			Sequence int
			Rows     []json.RawMessage
			Count    int    `json:"row_count"`
			SHA      string `json:"sha256"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type == "chunk" {
			sequence++
			if sequence != frame.Sequence || len(frame.Rows) > 256 {
				t.Fatal("chunk bound")
			}
			for _, raw := range frame.Rows {
				count++
				digest.Write(raw)
				digest.Write([]byte{'\n'})
				var row RecognitionMetricRow
				if err := json.Unmarshal(raw, &row); err != nil || row.Kind != "recognized_income" || row.Status != "CONFIRMED" {
					t.Fatalf("invalid row %v", err)
				}
				sum += row.AmountMinor
			}
		}
		if frame.Type == "end" {
			ended = true
			if frame.Count != count || frame.SHA != hex.EncodeToString(digest.Sum(nil)) {
				t.Fatal("incomplete end")
			}
		}
	}
	if !ended || count != 10001 || sum != 50005000 {
		t.Fatalf("end=%v count=%d sum=%d", ended, count, sum)
	}
	if path := os.Getenv("LOCAL_METRIC_STREAM_OUTPUT_FILE"); path != "" {
		if err := os.WriteFile(path, out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Log(fmt.Sprintf("validated %d rows, %d bytes", count, out.Len()))
}
