package migrations

import (
	"strconv"
	"strings"
	"testing"
)

// TestMergedMigrationOrderPreservesPublishedHistory 验证迁移顺序连续且无新增编号冲突。
// 参数：t为测试上下文，历史两份000005按已发布完整文件名保留。
// 返回值：无；历史顺序改变、新编号重复、跳号或上传顺序错误时失败。
func TestMergedMigrationOrderPreservesPublishedHistory(t *testing.T) {
	entries, err := Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	previous := 0
	var tail []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		version, err := strconv.Atoi(name[:6])
		if err != nil {
			t.Fatal(err)
		}
		if version == 5 && name == "000005_report_exports.sql" && previous == 5 {
			continue
		}
		if version != previous+1 {
			t.Fatalf("migration %s follows %06d: duplicate or missing version", name, previous)
		}
		previous = version
		if version >= 22 {
			tail = append(tail, name)
		}
	}
	want := []string{
		"000022_central_session_governance.sql",
		"000023_red_flush_allocation_reversal_unique.sql",
		"000024_dunning_active_case_unique.sql",
		"000025_contract_revision_hold.sql",
		"000026_report_export_requester_scope.sql",
	}
	if strings.Join(tail, ",") != strings.Join(want, ",") {
		t.Fatalf("central session and financial migrations order = %v, want %v", tail, want)
	}
}
