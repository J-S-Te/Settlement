package sessiongovernance

import (
	"context"
	"testing"
	"time"
)

// TestRotationContextSurvivesPageCancellation 验证切页取消不能丢失已授权的单次刷新结果。
// 参数：t为上下文边界测试工具。
// 返回值：无；取消穿透、缺少有界期限或资源无法释放时失败。
func TestRotationContextSurvivesPageCancellation(t *testing.T) {
	request, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	rotation, cancelRotation := RotationContext(request)
	defer cancelRotation()
	if rotation.Err() != nil {
		t.Fatal("page cancellation must not discard an authorized token rotation")
	}
	deadline, ok := rotation.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= 14*time.Second || remaining > 15*time.Second {
		t.Fatal("token rotation must remain bounded to 15 seconds")
	}
	cancelRotation()
	if rotation.Err() != context.Canceled {
		t.Fatal("rotation cleanup must release its context")
	}
}
