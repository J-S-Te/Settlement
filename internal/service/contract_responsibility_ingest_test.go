package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// responsibilityEvent 构建无财务字段的责任事件。
// 参数：无。
// 返回值：合法责任事件。
func responsibilityEvent() ContractResponsibilityEvent {
	return ContractResponsibilityEvent{EventID: "R1", EventType: "contract.responsibility_changed.v1", TenantID: "tenant", SourceContractID: "C1", SourceVersion: 3, OwnerUserID: "owner-new", Active: true, OccurredAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)}
}

// TestResponsibilityRejectsUntrustedAndInvalid 验证非法责任事件触库前拒绝。
// 参数：t 为测试上下文。
// 返回值：无。
func TestResponsibilityRejectsUntrustedAndInvalid(t *testing.T) {
	for _, mutate := range []func(*ContractResponsibilityEvent){func(e *ContractResponsibilityEvent) { e.TenantID = "other" }, func(e *ContractResponsibilityEvent) { e.SourceVersion = 0 }, func(e *ContractResponsibilityEvent) { e.OwnerUserID = "" }, func(e *ContractResponsibilityEvent) { e.EventType = "unknown" }} {
		e := responsibilityEvent()
		mutate(&e)
		if _, err := (&Service{}).IngestContractResponsibility(context.Background(), "tenant", e); !errors.Is(err, ErrInvalid) {
			t.Fatalf("非法事件未拒绝：%v", err)
		}
	}
}

// TestResponsibilityStaleCannotRestoreOwner 验证迟到事件不会恢复旧责任。
// 参数：t 为测试上下文。
// 返回值：无。
func TestResponsibilityStaleCannotRestoreOwner(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_responsibility("},
		scriptdb.Step{Kind: "query", SQL: "FOR UPDATE", Rows: [][]driver.Value{{"owner-current", int64(5), false}}},
		scriptdb.Step{Kind: "query", SQL: "payload_hash", Columns: 1},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_responsibility_inbox"},
		scriptdb.Step{Kind: "commit"})
	applied, err := (&Service{DB: db}).IngestContractResponsibility(context.Background(), "tenant", responsibilityEvent())
	if err != nil || applied {
		t.Fatalf("迟到事件结果=%v,%v", applied, err)
	}
}

// TestResponsibilityNewVersionAppliesWithoutFinancialWrite 验证新版本仅更新责任投影。
// 参数：t 为测试上下文。
// 返回值：无。
func TestResponsibilityNewVersionAppliesWithoutFinancialWrite(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_responsibility("},
		scriptdb.Step{Kind: "query", SQL: "FOR UPDATE", Rows: [][]driver.Value{{"owner-old", int64(2), true}}},
		scriptdb.Step{Kind: "query", SQL: "payload_hash", Columns: 1},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_responsibility_inbox"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_contract_responsibility SET"}, scriptdb.Step{Kind: "commit"})
	applied, err := (&Service{DB: db}).IngestContractResponsibility(context.Background(), "tenant", responsibilityEvent())
	if err != nil || !applied {
		t.Fatalf("新事件结果=%v,%v", applied, err)
	}
}

// TestResponsibilitySameVersionConflictAndStorageFailure 验证同版本分叉与持久失败均不提交责任。
// 参数：t 为测试上下文。
// 返回值：无。
func TestResponsibilitySameVersionConflictAndStorageFailure(t *testing.T) {
	for _, failInbox := range []bool{false, true} {
		version := int64(3)
		if failInbox {
			version = 2
		}
		steps := []scriptdb.Step{{Kind: "begin"}, {Kind: "exec", SQL: "INSERT INTO settlement_contract_responsibility("}, {Kind: "query", SQL: "FOR UPDATE", Rows: [][]driver.Value{{"old-owner", version, true}}}, {Kind: "query", SQL: "payload_hash", Columns: 1}}
		failure := errors.New("inbox unavailable")
		if failInbox {
			steps = append(steps, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_responsibility_inbox", Err: failure})
		}
		steps = append(steps, scriptdb.Step{Kind: "rollback"})
		_, err := (&Service{DB: scriptdb.Open(t, steps...)}).IngestContractResponsibility(context.Background(), "tenant", responsibilityEvent())
		if failInbox && !errors.Is(err, failure) || !failInbox && !errors.Is(err, ErrConflict) {
			t.Fatalf("责任冲突/存储失败=%v", err)
		}
	}
}

// TestResponsibilityReusedEventIDRejectsChangedPayload 验证事件ID被不同内容复用时拒绝。
// 参数：t 为测试上下文。
// 返回值：无。
func TestResponsibilityReusedEventIDRejectsChangedPayload(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_contract_responsibility("}, scriptdb.Step{Kind: "query", SQL: "FOR UPDATE", Rows: [][]driver.Value{{"owner-current", int64(5), false}}}, scriptdb.Step{Kind: "query", SQL: "payload_hash", Rows: [][]driver.Value{{[]byte("different content")}}}, scriptdb.Step{Kind: "rollback"})
	_, err := (&Service{DB: db}).IngestContractResponsibility(context.Background(), "tenant", responsibilityEvent())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("事件ID复用未拒绝：%v", err)
	}
}
