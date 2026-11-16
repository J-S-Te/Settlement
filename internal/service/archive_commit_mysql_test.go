package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// TestArchiveOperationMySQLUnknownCommit 验证真实归档提交响应丢失后不释放已归档文件。
// 参数：t为本轮明确授权的私有隔离MySQL上下文。
// 返回值：无，已提交文档被退休、释放或重放重传时失败。
func TestArchiveOperationMySQLUnknownCommit(t *testing.T) {
	db := financeOperationMySQL(t)
	raw, err := os.ReadFile(os.Getenv("SETTLEMENT_FINANCE_TEST_DSN_FILE"))
	if err != nil {
		t.Fatal("isolated DSN unavailable")
	}
	cfg, err := mysql.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal("invalid isolated DSN")
	}
	base, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal("isolated connector unavailable")
	}
	wrapped := sql.OpenDB(&lostCommitConnector{Connector: base})
	defer wrapped.Close()
	svc := &Service{DB: wrapped}
	tenant := "unknown-commit-" + newID()
	p := archivePrincipal(tenant)
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "app", GatewayURL: "http://isolated"}
	invoice := seedArchiveInvoice(t, db, tenant)
	input := archiveInput(invoice, "unknown-commit")
	g := &archiveOperationGateway{}
	if _, _, err := svc.UploadInvoiceDocument(context.Background(), p, identity, g, input); err == nil {
		t.Fatal("lost COMMIT response hidden")
	}
	if state := archiveState(t, db, invoice); state != "ARCHIVED" {
		t.Fatalf("committed archive state=%s", state)
	}
	worker := InvoiceArchiveWorker{DB: db, Identity: identity, Gateway: g}
	if found, err := worker.recoverOne(context.Background()); found || err != nil {
		t.Fatalf("committed effect retired found=%v err=%v", found, err)
	}
	if _, created, err := (&Service{DB: db}).UploadInvoiceDocument(context.Background(), p, identity, g, input); err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
	if g.upload != 1 || g.release != 0 {
		t.Fatalf("uploads=%d releases=%d", g.upload, g.release)
	}
}

// TestEvidenceOperationMySQLBindAndCommitRetry 验证上传成功但绑定或审计提交失败后按稳定请求身份重试。
// 参数：t为本轮明确授权的私有隔离MySQL上下文。
// 返回值：无，失败伪装READY、重复对象或重放重复上传时失败。
func TestEvidenceOperationMySQLBindAndCommitRetry(t *testing.T) {
	db := financeOperationMySQL(t)
	tenant := "evidence-retry-" + newID()
	p := recognitionTestPrincipal(tenant, "maker", "settlement_accountant")
	svc := &Service{DB: db, RecognitionDirectory: recognitionTestDirectory{}}
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "app", GatewayURL: "http://isolated"}
	input := RecognitionEvidenceInput{OrganizationID: "org", OrganizationWitnessID: "witness", Name: "retry.pdf", MediaType: "application/pdf", Content: []byte("%PDF-1.4\nretry evidence")}
	gateway := &recognitionEvidenceGateway{bindFails: true}
	if _, err := svc.UploadRecognitionEvidence(context.Background(), p, "retry", identity, gateway, input); !errors.Is(err, ErrDependency) {
		t.Fatalf("bind failure hidden: %v", err)
	}
	gateway.bindFails = false
	// 包装真实驱动只回滚一次READY事务，不需要提升隔离数据库全局权限。
	raw, err := os.ReadFile(os.Getenv("SETTLEMENT_FINANCE_TEST_DSN_FILE"))
	if err != nil {
		t.Fatal("isolated DSN unavailable")
	}
	cfg, err := mysql.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal("invalid isolated DSN")
	}
	base, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal("isolated connector unavailable")
	}
	wrapped := sql.OpenDB(&financeRollbackConnector{Connector: base, fail: true})
	defer wrapped.Close()
	svc.DB = wrapped
	if _, err := svc.UploadRecognitionEvidence(context.Background(), p, "retry", identity, gateway, input); err == nil {
		t.Fatal("audit failure hidden")
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM settlement_recognition_evidence WHERE tenant_id=?`, tenant).Scan(&status); err != nil || status != "PENDING" {
		t.Fatalf("failed commit status=%s err=%v", status, err)
	}
	evidence, err := svc.UploadRecognitionEvidence(context.Background(), p, "retry", identity, gateway, input)
	if err != nil || evidence.Status != "READY" {
		t.Fatalf("retry=%+v err=%v", evidence, err)
	}
	if _, err := svc.UploadRecognitionEvidence(context.Background(), p, "retry", identity, gateway, input); err != nil {
		t.Fatal(err)
	}
	if gateway.uploadCalls != 3 {
		t.Fatalf("READY replay repeated upload: %d", gateway.uploadCalls)
	}
}

type financeRollbackConnector struct {
	driver.Connector
	mu   sync.Mutex
	fail bool
}
type financeRollbackConn struct {
	driver.Conn
	owner *financeRollbackConnector
	ready bool
}
type financeRollbackTx struct {
	driver.Tx
	conn *financeRollbackConn
}

// Connect 包装真实MySQL连接以注入一次归档事务回滚。
// 参数：ctx为真实连接上下文。
// 返回值：包装连接或原数据库连接错误。
func (c *financeRollbackConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	return &financeRollbackConn{Conn: conn, owner: c}, err
}

// BeginTx 启动真实事务并重置READY提交标记。
// 参数：ctx/options为事务上下文及隔离选项。
// 返回值：真实事务包装及数据库错误。
func (c *financeRollbackConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.ready = false
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	return &financeRollbackTx{Tx: tx, conn: c}, err
}

// Prepare 识别真正的证据READY写入阶段。
// 参数：query为真实SQL语句。
// 返回值：真实数据库语句及准备错误。
func (c *financeRollbackConn) Prepare(query string) (driver.Stmt, error) {
	if strings.Contains(query, "SET status='READY'") {
		c.ready = true
	}
	return c.Conn.Prepare(query)
}

// Commit 对第一次真实READY事务执行回滚，后续事务提交原样传递。
// 参数：无。
// 返回值：注入的提交失败或真实提交错误。
func (tx *financeRollbackTx) Commit() error {
	c := tx.conn
	c.owner.mu.Lock()
	fail := c.ready && c.owner.fail
	if fail {
		c.owner.fail = false
	}
	c.owner.mu.Unlock()
	if fail {
		return errors.Join(ErrDependency, tx.Tx.Rollback())
	}
	return tx.Tx.Commit()
}
