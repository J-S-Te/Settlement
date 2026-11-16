package service

import (
	"database/sql"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/migration"
	"github.com/j-s-te/settlement/migrations"
)

// financeOperationMySQL 仅连接本轮明确授权的回环私有测试库并应用源码迁移。
// 参数：t为测试上下文，DSN只从SETTLEMENT_FINANCE_TEST_DSN_FILE指定的私有普通文件读取。
// 返回值：本轮local_fix_settlement隔离连接，未配置时跳过，越界配置直接拒绝。
func financeOperationMySQL(t *testing.T) *sql.DB {
	t.Helper()
	path := os.Getenv("SETTLEMENT_FINANCE_TEST_DSN_FILE")
	if path == "" {
		t.Skip("SETTLEMENT_FINANCE_TEST_DSN_FILE required; no business database accessed")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private regular isolated DSN file required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read isolated DSN failed")
	}
	dsn := strings.TrimSpace(string(raw))
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid isolated DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || host != "127.0.0.1" || cfg.DBName != "local_fix_settlement" {
		t.Fatal("explicit loopback local_fix_settlement database required")
	}
	if err = migration.Run(t.Context(), dsn, migrations.Files); err != nil {
		t.Fatal("isolated finance migration failed")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open isolated finance database failed")
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
