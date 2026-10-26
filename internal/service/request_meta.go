package service

import (
	"context"
)

func WithRequestMeta(ctx context.Context, meta RequestMeta) context.Context {
	return context.WithValue(ctx, requestMetaKey{}, meta)
}

// RequestMetaFrom 读取请求来源信息；没有时返回零值。
func RequestMetaFrom(ctx context.Context) RequestMeta {
	meta, _ := ctx.Value(requestMetaKey{}).(RequestMeta)
	return meta
}

// enrichAuditIP 把来源地址并入审计 detail；业务已显式提供时不覆盖。
func enrichAuditIP(payload map[string]any, ctx context.Context) {
	meta := RequestMetaFrom(ctx)
	if _, ok := payload["login_ip"]; !ok && meta.LoginIP != "" {
		payload["login_ip"] = meta.LoginIP
	}
	if _, ok := payload["client_ip"]; !ok && meta.ClientIP != "" {
		payload["client_ip"] = meta.ClientIP
	}
	if _, ok := payload["ip_source"]; !ok && meta.IPSource != "" {
		payload["ip_source"] = meta.IPSource
	}
}

// enqueueCreditReversal 撤销应收当前结清轮次的
// 结清事实。完成事件按应收及事件类型定位，
// 而非按被冲销分配定位：多个分配共同
// 结清一期时，完成事实属于关闭该期的分配，
// 冲销则可能针对较早的分配。
//
// 尚未投递的完成事件被取消。已投递或进行中的
// 完成事实由冲销事件补偿，该事件引用
// 原事件及真正关闭该期的分配。
// 已有冲销事件的完成事实绝不再次补偿，
// 确保重复或重叠冲销仍保持幂等。
