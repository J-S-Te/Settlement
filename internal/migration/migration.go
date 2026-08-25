package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	driver "github.com/go-sql-driver/mysql"
)

func Run(ctx context.Context, dsn string, files embed.FS) error {
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		return err
	}
	cfg.MultiStatements = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.PingContext(ctx); err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS settlement_schema_migration (name VARCHAR(255) PRIMARY KEY, checksum BINARY(32) NOT NULL, applied_at DATETIME(3) NOT NULL) ENGINE=InnoDB`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := files.ReadFile(entry.Name())
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		var existing []byte
		err = db.QueryRowContext(ctx, "SELECT checksum FROM settlement_schema_migration WHERE name = ?", entry.Name()).Scan(&existing)
		if err == nil {
			if string(existing) != string(sum[:]) {
				return fmt.Errorf("migration %s checksum changed", entry.Name())
			}
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err = db.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", entry.Name(), err)
		}
		if _, err = db.ExecContext(ctx, "INSERT INTO settlement_schema_migration(name,checksum,applied_at) VALUES(?,?,UTC_TIMESTAMP(3))", entry.Name(), sum[:]); err != nil {
			return err
		}
	}
	return nil
}
