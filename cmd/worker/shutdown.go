package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultShutdownDrain 是收到停止信号后等待后台组件自行收尾的默认预算。
// 可用 SETTLEMENT_SHUTDOWN_DRAIN_SECONDS 覆盖；缺失、非法或 <=0 时保持默认，
// 不新增必填配置。
const defaultShutdownDrain = 20 * time.Second

// loopGroup 收拢 worker 的后台循环，记录仍在运行的组件名，
// 便于停机超出排空预算时如实列出未完成项，并保证所有循环都被 join。
type loopGroup struct {
	wg      sync.WaitGroup
	mu      sync.Mutex
	order   []string
	running map[string]bool
}

func newLoopGroup() *loopGroup {
	return &loopGroup{running: map[string]bool{}}
}

// Go 启动一个受 join 管理的后台循环。
// 参数：name 为退出超预算时上报的组件名，run 为循环本体（应在 ctx 取消后返回）。
func (g *loopGroup) Go(name string, run func()) {
	g.mu.Lock()
	g.order = append(g.order, name)
	g.running[name] = true
	g.mu.Unlock()
	g.wg.Add(1)
	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.running, name)
			g.mu.Unlock()
			g.wg.Done()
		}()
		run()
	}()
}

// Wait 阻塞直到所有已启动循环返回。
func (g *loopGroup) Wait() { g.wg.Wait() }

// Unfinished 返回仍在运行的组件名，按启动顺序排列。
func (g *loopGroup) Unfinished() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	names := make([]string, 0, len(g.running))
	for _, name := range g.order {
		if g.running[name] {
			names = append(names, name)
		}
	}
	return names
}

// drainWait 在预算内等待后台循环收尾。
// 参数：budget 为等待上限，wait 通常传 loopGroup.Wait。
// 返回值：全部在预算内退出返回 true；超预算立即返回 false，由调用方列出未完成组件后退出。
func drainWait(budget time.Duration, wait func()) (done bool) {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		wait()
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-finished:
		return true
	case <-timer.C:
		return false
	}
}

// shutdownDrainBudget 读取 SETTLEMENT_SHUTDOWN_DRAIN_SECONDS；
// 缺失、非法或 <=0 时使用默认 20s 预算并告警。
func shutdownDrainBudget() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SETTLEMENT_SHUTDOWN_DRAIN_SECONDS"))
	if raw == "" {
		return defaultShutdownDrain
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		slog.Warn("invalid SETTLEMENT_SHUTDOWN_DRAIN_SECONDS; using the default drain budget",
			"value", raw, "default_seconds", int(defaultShutdownDrain/time.Second))
		return defaultShutdownDrain
	}
	return time.Duration(seconds) * time.Second
}
