package service

import "testing"

// TestImportExportIDBindsTenantSnapshotUserAndKey 验证重放主键只对同一租户、快照、申请人和键稳定。
// 参数：t为测试上下文。
// 返回值：无，长度不符或任一维度变化仍命中同一主键时失败。
func TestImportExportIDBindsTenantSnapshotUserAndKey(t *testing.T) {
	const tenant, snapshot, user, key = "tenant-a", "11111111111111111111111111111111", "finance", "export-key"
	id := importExportID(tenant, snapshot, user, key)
	if len(id) != 32 || id != importExportID(tenant, snapshot, user, key) {
		t.Fatalf("id must be stable 32 hex characters, got %q", id)
	}
	for name, other := range map[string]string{
		"tenant":   importExportID("tenant-b", snapshot, user, key),
		"snapshot": importExportID(tenant, "22222222222222222222222222222222", user, key),
		"user":     importExportID(tenant, snapshot, "other-user", key),
		"key":      importExportID(tenant, snapshot, user, "other-key"),
	} {
		if other == id {
			t.Fatalf("%s change reused the same export id %q", name, id)
		}
	}
}
