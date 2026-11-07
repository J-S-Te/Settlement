package main

import (
	"testing"
	"time"
)

// TestDrainWaitReturnsTrueWhenLoopsExitWithinBudget 验证正常收尾不额外延迟退出。
func TestDrainWaitReturnsTrueWhenLoopsExitWithinBudget(t *testing.T) {
	loops := newLoopGroup()
	loops.Go("report_export", func() {})
	if done := drainWait(2*time.Second, loops.Wait); !done {
		t.Fatal("drain wait reported timeout although every loop returned")
	}
	if unfinished := loops.Unfinished(); len(unfinished) != 0 {
		t.Fatalf("finished loops still reported as unfinished: %v", unfinished)
	}
}

// TestDrainWaitReportsTimeoutAndUnfinishedComponents 验证超预算返回 false 并可列出未退出组件名。
func TestDrainWaitReportsTimeoutAndUnfinishedComponents(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	loops := newLoopGroup()
	loops.Go("outbox_worker", func() { <-release })
	loops.Go("commercial_license", func() { <-release })

	start := time.Now()
	done := drainWait(50*time.Millisecond, loops.Wait)
	if done {
		t.Fatalf("drain wait ignored components that never returned (elapsed=%s)", time.Since(start))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("drain wait exceeded its budget by too much: %s", elapsed)
	}
	unfinished := loops.Unfinished()
	if len(unfinished) != 2 || unfinished[0] != "outbox_worker" || unfinished[1] != "commercial_license" {
		t.Fatalf("unfinished components=%v want [outbox_worker commercial_license]", unfinished)
	}
}

// TestShutdownDrainBudgetFallsBackToDefault 验证缺失、非法与 <=0 均使用默认 20s 预算。
func TestShutdownDrainBudgetFallsBackToDefault(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"", defaultShutdownDrain},
		{"5", 5 * time.Second},
		{" 7 ", 7 * time.Second},
		{"0", defaultShutdownDrain},
		{"-3", defaultShutdownDrain},
		{"abc", defaultShutdownDrain},
	}
	for _, c := range cases {
		t.Run("value="+c.value, func(t *testing.T) {
			t.Setenv("SETTLEMENT_SHUTDOWN_DRAIN_SECONDS", c.value)
			if got := shutdownDrainBudget(); got != c.want {
				t.Fatalf("budget=%s want %s", got, c.want)
			}
		})
	}
	if defaultShutdownDrain != 20*time.Second {
		t.Fatalf("default drain budget=%s want 20s", defaultShutdownDrain)
	}
}
