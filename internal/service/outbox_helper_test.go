package service

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"testing"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestOutboxAuditSharesEnrichedBodyWithOutbox 验证审计与 outbox 使用同一份
// 富化后的 body：两类来源地址都写入，payload 的 result 透传到本地审计。
// 参数：t 为测试上下文。
// 返回值：无；IP 丢失、body 不一致或 result 被硬编码为 SUCCESS 时失败。
func TestOutboxAuditSharesEnrichedBodyWithOutbox(t *testing.T) {
	var outboxArgs, auditArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event", Args: &outboxArgs},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event", Args: &auditArgs},
		scriptdb.Step{Kind: "commit"},
	)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithRequestMeta(context.Background(), RequestMeta{LoginIP: "10.0.0.1", ClientIP: "10.0.0.2", IPSource: "HEADER"})
	payload := map[string]any{"actor_id": "user-1", "result": "FAILURE", "risk_level": "HIGH"}
	if err = outbox(ctx, tx, "tenant", "PLATFORM_AUDIT", "SETTLEMENT_RECEIPT_RECORDED", "receipt", "receipt-1", payload); err != nil {
		t.Fatalf("outbox: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	outboxBody, _ := outboxArgs[7].Value.([]byte)
	var detail map[string]any
	if err = json.Unmarshal(outboxBody, &detail); err != nil {
		t.Fatalf("outbox payload is not JSON: %v", err)
	}
	for field, want := range map[string]string{"login_ip": "10.0.0.1", "client_ip": "10.0.0.2", "ip_source": "HEADER", "result": "FAILURE"} {
		if got, _ := detail[field].(string); got != want {
			t.Fatalf("outbox payload %s=%q, want %q; payload=%s", field, got, want, outboxBody)
		}
	}
	if got := auditArgs[2].Value; got != "user-1" {
		t.Fatalf("audit actor=%v, want user-1", got)
	}
	if got := auditArgs[6].Value; got != "FAILURE" {
		t.Fatalf("audit result=%v, want FAILURE from payload", got)
	}
	auditBody, _ := auditArgs[9].Value.([]byte)
	if string(auditBody) != string(outboxBody) {
		t.Fatalf("audit and outbox must share the enriched body: audit=%s outbox=%s", auditBody, outboxBody)
	}
}

// TestOutboxAuditDefaultsToSuccessAndKeepsExplicitIP 验证 payload 缺省 result 时
// 本地审计回落 SUCCESS，且业务显式提供的地址不被请求来源覆盖。
// 参数：t 为测试上下文。
// 返回值：无；默认结果或显式地址被改写时失败。
func TestOutboxAuditDefaultsToSuccessAndKeepsExplicitIP(t *testing.T) {
	var auditArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event", Args: &auditArgs},
		scriptdb.Step{Kind: "commit"},
	)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithRequestMeta(context.Background(), RequestMeta{ClientIP: "10.0.0.2", IPSource: "HEADER"})
	payload := map[string]any{"actor_id": "user-1", "client_ip": "203.0.113.9"}
	if err = outbox(ctx, tx, "tenant", "PLATFORM_AUDIT", "SETTLEMENT_RECEIPT_RECORDED", "receipt", "receipt-1", payload); err != nil {
		t.Fatalf("outbox: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := auditArgs[6].Value; got != "SUCCESS" {
		t.Fatalf("audit result=%v, want SUCCESS when payload omits result", got)
	}
	auditBody, _ := auditArgs[9].Value.([]byte)
	var detail map[string]any
	if err = json.Unmarshal(auditBody, &detail); err != nil {
		t.Fatalf("audit detail is not JSON: %v", err)
	}
	if got, _ := detail["client_ip"].(string); got != "203.0.113.9" {
		t.Fatalf("explicit client_ip was overwritten: %q", got)
	}
	if got, _ := detail["ip_source"].(string); got != "HEADER" {
		t.Fatalf("ip_source=%q, want HEADER", got)
	}
}
