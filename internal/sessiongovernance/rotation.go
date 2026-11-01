package sessiongovernance

import (
	"context"
	"time"
)

// RotationContext 为已验证会话的凭据轮换提供不受切页取消影响的有界提交窗口。
// 参数：ctx为已验证请求上下文，保留追踪信息；不能用来执行业务操作。
// 返回值：最多15秒的轮换上下文及必须调用的释放函数，原八小时截止仍独立核验。
func RotationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
}
