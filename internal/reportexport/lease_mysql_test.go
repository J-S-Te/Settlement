package reportexport

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/migrations"
)

// 仅允许显式配置的隔离测试服务器。创建密码学随机命名的全新
// 数据库，并由本轮运行独占所有。
func exportMySQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_TEST_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_TEST_DSN is not configured; real MySQL export acceptance not executed")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !regexp.MustCompile(`^settlement_e2e(_[a-z0-9_]+)?$`).MatchString(cfg.DBName) {
		t.Fatal("SETTLEMENT_TEST_DSN must name settlement_e2e or settlement_e2e_<namespace> (DSN withheld)")
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal("generate test database identity failed")
	}
	name := "settlement_e2e_export_" + hex.EncodeToString(nonce[:])
	controlCfg := cfg.Clone()
	controlCfg.DBName = ""
	control, err := sql.Open("mysql", controlCfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated test control connection failed")
	}
	t.Cleanup(func() { _ = control.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err = control.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create owned export test database failed (DSN withheld)")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := control.ExecContext(cleanup, "DROP DATABASE `"+name+"`"); err != nil {
			t.Error("drop owned export test database failed (DSN withheld)")
		}
	})
	runCfg := cfg.Clone()
	runCfg.DBName = name
	runCfg.MultiStatements = true
	runCfg.ParseTime = true
	db, err := sql.Open("mysql", runCfg.FormatDSN())
	if err != nil {
		t.Fatal("open owned export test database failed")
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, file := range []string{"000005_report_exports.sql", "000007_file_gateway_report_exports.sql", "000014_report_export_effects.sql"} {
		body, err := migrations.Files.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply export migration %s failed: %v", file, err)
		}
	}
	return db
}

func TestMySQLExportRenewalAndGenerationFencing(t *testing.T) {
	db := exportMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, `INSERT INTO settlement_report_export_job(id,tenant_id,requested_by,idempotency_key,report_type,status,created_at,updated_at) VALUES('job1','tenant','user','key',?,'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, InvoicedReceivablesCSV)
	if err != nil {
		t.Fatal(err)
	}
	old := &Worker{DB: db, TenantID: "tenant", WorkerID: "same-restarted-worker"}
	item, err := old.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err = old.renew(ctx, db, item); err != nil {
			t.Fatalf("back-to-back live renewal: %v", err)
		}
	}
	if _, err = db.ExecContext(ctx, `UPDATE settlement_report_export_job SET locked_until=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 SECOND) WHERE id='job1'`); err != nil {
		t.Fatal(err)
	}
	if err = old.renew(ctx, db, item); !errors.Is(err, errLeaseLost) {
		t.Fatalf("expired renewal=%v", err)
	}
	newWorker := &Worker{DB: db, TenantID: "tenant", WorkerID: old.WorkerID}
	next, err := newWorker.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.generation != item.generation+1 {
		t.Fatalf("takeover did not advance fencing: old=%d new=%d", item.generation, next.generation)
	}
	called := false
	if err = old.leased(ctx, item, func(*sql.Tx) error { called = true; return nil }); !errors.Is(err, errLeaseLost) || called {
		t.Fatalf("old generation reached side effect: called=%v err=%v", called, err)
	}
	if err = newWorker.renew(ctx, db, next); err != nil {
		t.Fatalf("new generation renewal=%v", err)
	}

	// 进行中的外部调用与接管持有同一行锁。
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- newWorker.leased(ctx, next, func(*sql.Tx) error { close(entered); <-release; return nil })
	}()
	<-entered
	_, claimErr := old.claim(ctx)
	close(release)
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(claimErr, sql.ErrNoRows) {
		t.Fatalf("locked active export was reclaimed: %v", claimErr)
	}
}
