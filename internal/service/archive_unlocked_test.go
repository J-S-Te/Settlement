package service

import (
	"context"
	"database/sql/driver"
	"io"
	"testing"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

type unlockedArchiveGateway struct {
	t      *testing.T
	active *bool
}

// Upload 检查网关上传不占用数据库事务。
// 参数：上下文及上传身份与内容为测试网关边界。
// 返回值：稳定测试文件及空错误。
func (g unlockedArchiveGateway) Upload(context.Context, string, string, string, string, string, io.Reader) (string, error) {
	if *g.active {
		g.t.Error("gateway upload held a database transaction")
	}
	return "file", nil
}

// Bind 检查网关绑定不占用数据库事务。
// 参数：上下文及绑定身份为测试网关边界。
// 返回值：空错误。
func (g unlockedArchiveGateway) Bind(context.Context, string, string, string, string, string, string) error {
	if *g.active {
		g.t.Error("gateway bind held a database transaction")
	}
	return nil
}

// RetireUpload 检查恢复退休不占用数据库事务。
// 参数：上下文及稳定请求身份为测试网关边界。
// 返回值：已退休文件及就绪状态。
func (g unlockedArchiveGateway) RetireUpload(context.Context, string) (string, string, error) {
	if *g.active {
		g.t.Error("gateway retirement held a database transaction")
	}
	return "file", "READY", nil
}

// Release 检查恢复释放不占用数据库事务。
// 参数：上下文及精确绑定身份为测试网关边界。
// 返回值：空错误。
func (g unlockedArchiveGateway) Release(context.Context, string, string, string, string, string) error {
	if *g.active {
		g.t.Error("gateway release held a database transaction")
	}
	return nil
}

// TestInvoiceRecoveryGatewayOutsideTransaction 验证恢复先短事务领取，再事务外调用网关。
// 参数：t为SQL事务边界测试上下文；不代表真实MySQL验收。
// 返回值：无，持锁外呼或缺少操作代次保护时失败。
func TestInvoiceRecoveryGatewayOutsideTransaction(t *testing.T) {
	active := false
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin", After: func() { active = true }},
		scriptdb.Step{Kind: "query", SQL: "FOR UPDATE SKIP LOCKED", Rows: [][]driver.Value{{"effect", "invoice", "ELECTRONIC_INVOICE", "sum", "invoice.pdf", "application/pdf", "app", "origin", "request", "", "", "PENDING"}}},
		scriptdb.Step{Kind: "exec", SQL: "operation_token="},
		scriptdb.Step{Kind: "commit", After: func() { active = false }},
		scriptdb.Step{Kind: "begin", After: func() { active = true }},
		scriptdb.Step{Kind: "exec", SQL: "state='RETIRED'"},
		scriptdb.Step{Kind: "commit", After: func() { active = false }},
	)
	w := InvoiceArchiveWorker{DB: db, Identity: InvoiceArchiveIdentity{TenantID: "tenant", ApplicationID: "app", GatewayURL: "origin"}, Gateway: unlockedArchiveGateway{t, &active}}
	if found, err := w.recoverOne(context.Background()); !found || err != nil {
		t.Fatalf("found=%v err=%v", found, err)
	}
}
