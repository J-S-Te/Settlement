package service

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// 若集成测试再次开始读取业务配置，格式错误的业务 DSN
// 会在任何网络访问前失败。
func TestCreditSyncMySQLIgnoresBusinessDSN(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestCreditSyncWorkerClaimsRetriesAndDelivers$", "-test.v")
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "USER", "LOGNAME", "LANG", "LC_ALL", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "SETTLEMENT_MYSQL_DSN=invalid-business-dsn")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worker test must skip without an explicit test DSN: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "--- SKIP: TestCreditSyncWorkerClaimsRetriesAndDelivers") {
		t.Fatalf("worker test did not report the required database skip:\n%s", output)
	}
}

func TestCreditSyncMySQLRejectsNonTestDatabaseBeforeConnecting(t *testing.T) {
	for _, dsn := range []string{
		"", "invalid-dsn", "user:secret@tcp(127.0.0.1:1)/settlement",
		"user:secret@tcp(127.0.0.1:1)/", "user:secret@tcp(127.0.0.1:1)/settlement_e2e_prod`",
		"user:secret@tcp(127.0.0.1:1)/production_settlement_e2e",
	} {
		cfg, err := creditSyncMySQLTestConfig(dsn)
		if err == nil || cfg != nil {
			t.Fatal("unsafe or absent test database must be rejected before opening a connection")
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatal("validation error disclosed DSN material")
		}
	}
	for _, name := range []string{"settlement_e2e", "settlement_e2e_ci_1"} {
		cfg, err := creditSyncMySQLTestConfig("user:secret@tcp(127.0.0.1:1)/" + name)
		if err != nil || cfg.DBName != name {
			t.Fatal("explicit test database must pass validation without contacting MySQL")
		}
	}
}

func TestCreditSyncMySQLRunNamespacesAreUniqueAndSafe(t *testing.T) {
	seen := make(map[string]bool)
	valid := regexp.MustCompile(`^settlement_e2e_[a-f0-9]{24}$`)
	for range 100 {
		name, err := creditSyncMySQLRunDatabase()
		if err != nil {
			t.Fatal(err)
		}
		if !valid.MatchString(name) || seen[name] {
			t.Fatal("each run must own a fresh, SQL-safe database namespace")
		}
		seen[name] = true
	}
}
