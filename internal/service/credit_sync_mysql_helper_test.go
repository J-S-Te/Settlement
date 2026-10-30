package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"github.com/j-s-te/settlement/internal/migration"
	"github.com/j-s-te/settlement/migrations"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func creditSyncMySQLTestConfig(dsn string) (*mysql.Config, error) {
	cfg, err := mysql.ParseDSN(strings.TrimSpace(dsn))
	if err != nil || !regexp.MustCompile(`^settlement_e2e(_[a-z0-9_]+)?$`).MatchString(cfg.DBName) {
		// 驱动解析错误可能包含 DSN 值。测试输出必须排除凭据，
		// 包括格式错误或误传业务 DSN 的情况。
		return nil, errors.New("explicit MySQL test DSN must name settlement_e2e or settlement_e2e_<namespace>")
	}
	return cfg, nil
}

func creditSyncMySQLRunDatabase() (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", errors.New("generate isolated MySQL test namespace")
	}
	return "settlement_e2e_" + hex.EncodeToString(nonce[:]), nil
}

// newCreditSyncMySQLTestDB 新建独占信用同步测试库，避免跨租户worker认领其他任务记录。
// 参数：t为测试上下文，只允许显式隔离DSN。
// 返回值：独占连接及测试库名；不迁移或清理既有数据库。
func newCreditSyncMySQLTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_E2E_TEST_DSN"))
	if dsn == "" {
		t.Skip("SETTLEMENT_E2E_TEST_DSN is required for an isolated MySQL test")
	}
	cfg, err := creditSyncMySQLTestConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name, err := creditSyncMySQLRunDatabase()
	if err != nil {
		t.Fatal(err)
	}
	// 连接显式配置的服务器，不选择传入的
	// 测试数据库。CREATE 必须在不使用 IF NOT EXISTS 时成功，确保
	// 绝不能接管既有数据库，或之后将其作为自有数据库删除。
	controlCfg := cfg.Clone()
	controlCfg.DBName = ""
	controlDB, err := sql.Open("mysql", controlCfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated MySQL test control connection failed (DSN withheld)")
	}
	t.Cleanup(func() { _ = controlDB.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err = controlDB.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create isolated MySQL test database failed; dedicated test credentials need CREATE/DROP privileges (DSN withheld)")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := controlDB.ExecContext(cleanupCtx, "DROP DATABASE `"+name+"`"); err != nil {
			t.Errorf("drop owned MySQL test database %s failed (DSN withheld)", name)
		}
	})
	runCfg := cfg.Clone()
	runCfg.DBName = name
	runCfg.MultiStatements = true // 在下方应用真实信用同步迁移。
	db, err := sql.Open("mysql", runCfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated MySQL test database failed (DSN withheld)")
	}
	t.Cleanup(func() { _ = db.Close() })
	var selected string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&selected); err != nil || selected != name {
		t.Fatal("MySQL test connection is not bound to the owned database (DSN withheld)")
	}
	if err := migration.Run(ctx, runCfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatalf("apply complete settlement test migrations: %v", err)
	}

	return db, strings.TrimPrefix(name, "settlement_e2e_")
}
