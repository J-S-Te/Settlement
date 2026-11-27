// Package authorizationrefresh 传递只读会话页面的授权强制重算意图。
package authorizationrefresh

import (
	"context"
	"net/http"
	"strings"
)

// Header 是会话页面向平台传播的新鲜授权读取标记。
const Header = "X-Authorization-Refresh"

type forceKey struct{}

// FromRequest 只接受 GET 会话自描述路径上的显式刷新标记。
// 参数：ctx 为原调用上下文，request 为已路由的 HTTP 请求。
// 返回值：携带强制读取意图的上下文；其他请求保持原上下文。
func FromRequest(ctx context.Context, request *http.Request) context.Context {
	if request == nil || request.Method != http.MethodGet || request.Header.Get(Header) != "1" {
		return ctx
	}
	path := request.URL.Path
	if strings.HasSuffix(path, "/auth/me") || strings.HasSuffix(path, "/auth/session") {
		return context.WithValue(ctx, forceKey{}, true)
	}
	return ctx
}

// Forced 判断当前调用是否要求重新读取授权事实源。
// 参数：ctx 为经过 HTTP 入口解析的请求上下文。
// 返回值：显式强制读取时为 true，默认使用既有缓存策略。
func Forced(ctx context.Context) bool {
	forced, _ := ctx.Value(forceKey{}).(bool)
	return forced
}
