package config

import (
	"testing"
	"time"
)

// TestProjectReferenceSyncSettingsDefaults 验证未配置时默认 10 分钟、批量 50，
// 与 .env.example 的默认值一致。
// 参数：t 为测试上下文。
// 返回值：无；默认值或批量上限判定不符时失败。
func TestProjectReferenceSyncSettingsDefaults(t *testing.T) {
	t.Setenv("PROJECT_REFERENCE_SYNC_INTERVAL", "")
	t.Setenv("PROJECT_REFERENCE_SYNC_BATCH_SIZE", "")
	interval, batch, err := ProjectReferenceSyncSettings()
	if err != nil || interval != 10*time.Minute || batch != 50 {
		t.Fatalf("interval=%v batch=%d err=%v", interval, batch, err)
	}
}

// TestProjectReferenceSyncSettingsZeroDisables 验证显式 0 关闭刷新器，
// 而不是回落到默认周期。
// 参数：t 为测试上下文。
// 返回值：无；0 未被识别为关闭时失败。
func TestProjectReferenceSyncSettingsZeroDisables(t *testing.T) {
	t.Setenv("PROJECT_REFERENCE_SYNC_INTERVAL", "0")
	t.Setenv("PROJECT_REFERENCE_SYNC_BATCH_SIZE", "20")
	interval, batch, err := ProjectReferenceSyncSettings()
	if err != nil || interval != 0 || batch != 20 {
		t.Fatalf("interval=%v batch=%d err=%v", interval, batch, err)
	}
}

// TestProjectReferenceSyncSettingsRejectsInvalidValues 验证非法周期与越界批量
// 在启动期失败关闭，避免静默退回默认值。
// 参数：t 为测试上下文。
// 返回值：无；非法配置未被拒绝时失败。
func TestProjectReferenceSyncSettingsRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct{ interval, batch string }{
		{"every-ten-minutes", "50"},
		{"-5m", "50"},
		{"10m", "0"},
		{"10m", "201"},
		{"10m", "many"},
	} {
		t.Setenv("PROJECT_REFERENCE_SYNC_INTERVAL", tc.interval)
		t.Setenv("PROJECT_REFERENCE_SYNC_BATCH_SIZE", tc.batch)
		if _, _, err := ProjectReferenceSyncSettings(); err == nil {
			t.Fatalf("interval=%q batch=%q 必须拒绝", tc.interval, tc.batch)
		}
	}
}

// TestProjectReferenceSyncSettingsBatchBounds 验证批量上限 200 可用、201 被拒绝。
// 参数：t 为测试上下文。
// 返回值：无；边界判定不符时失败。
func TestProjectReferenceSyncSettingsBatchBounds(t *testing.T) {
	t.Setenv("PROJECT_REFERENCE_SYNC_INTERVAL", "1h")
	t.Setenv("PROJECT_REFERENCE_SYNC_BATCH_SIZE", "200")
	if _, batch, err := ProjectReferenceSyncSettings(); err != nil || batch != MaxProjectReferenceSyncBatchSize {
		t.Fatalf("batch=%d err=%v", batch, err)
	}
	t.Setenv("PROJECT_REFERENCE_SYNC_BATCH_SIZE", "201")
	if _, _, err := ProjectReferenceSyncSettings(); err == nil {
		t.Fatal("批量 201 必须拒绝")
	}
}
