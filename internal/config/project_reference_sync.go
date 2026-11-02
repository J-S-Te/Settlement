package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// 项目引用快照周期刷新的默认值（BIZ-XAPI-05b）。
// 默认每 10 分钟刷新一次、每批 50 个最久未刷新的项目；
// 显式 PROJECT_REFERENCE_SYNC_INTERVAL=0 关闭刷新器，而不是回退默认值。
const (
	DefaultProjectReferenceSyncInterval  = 10 * time.Minute
	DefaultProjectReferenceSyncBatchSize = 50
	MaxProjectReferenceSyncBatchSize     = 200
)

// ProjectReferenceSyncSettings 解析 worker 侧项目引用快照刷新设置。
// 参数：无，从进程环境读取 PROJECT_REFERENCE_SYNC_INTERVAL 与
// PROJECT_REFERENCE_SYNC_BATCH_SIZE。
// 返回值：interval（0 表示关闭刷新器）、batch（1..200）、配置错误。
// 机器身份复用 SETTLEMENT_PROJECT_REFERENCE_*；本设置只控制周期与批量。
func ProjectReferenceSyncSettings() (time.Duration, int, error) {
	rawInterval := strings.TrimSpace(os.Getenv("PROJECT_REFERENCE_SYNC_INTERVAL"))
	if rawInterval == "" {
		rawInterval = DefaultProjectReferenceSyncInterval.String()
	}
	interval, err := time.ParseDuration(rawInterval)
	if err != nil {
		return 0, 0, fmt.Errorf("PROJECT_REFERENCE_SYNC_INTERVAL: %w", err)
	}
	if interval < 0 {
		return 0, 0, fmt.Errorf("PROJECT_REFERENCE_SYNC_INTERVAL must not be negative; use 0 to disable")
	}
	rawBatch := strings.TrimSpace(os.Getenv("PROJECT_REFERENCE_SYNC_BATCH_SIZE"))
	if rawBatch == "" {
		rawBatch = strconv.Itoa(DefaultProjectReferenceSyncBatchSize)
	}
	batch, err := strconv.Atoi(rawBatch)
	if err != nil {
		return 0, 0, fmt.Errorf("PROJECT_REFERENCE_SYNC_BATCH_SIZE: %w", err)
	}
	if batch < 1 || batch > MaxProjectReferenceSyncBatchSize {
		return 0, 0, fmt.Errorf("PROJECT_REFERENCE_SYNC_BATCH_SIZE must be between 1 and %d", MaxProjectReferenceSyncBatchSize)
	}
	return interval, batch, nil
}
