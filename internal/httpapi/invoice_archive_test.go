package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	core "github.com/J-S-Te/license-core"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/config"
)

// The database boundary deliberately fails the document write. The invariant is
// a committed recovery record before any gateway side effect, not mock success.
type archiveFailureConnector struct {
	durable                                         bool
	operationToken                                  string
	inTransaction, externalWithTransaction          bool
	intentPending, archivePending, archiveCommitted bool
	externalWithoutIntent                           bool
	effect                                          []driver.Value
	uploads                                         int
	binds, locks, commits                           int
	failDocument                                    bool
	onLock                                          func(int)
	onUpload, onDocument, onIntent                  func()
}
type archiveFailureConn struct{ c *archiveFailureConnector }

func (c *archiveFailureConnector) Connect(context.Context) (driver.Conn, error) {
	return &archiveFailureConn{c}, nil
}
func (c *archiveFailureConnector) Driver() driver.Driver { return taxDetailDriver{} }
func (c *archiveFailureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
// Close 关闭不持外部资源的隔离SQL测试连接。
// 参数：无。
// 返回值：始终为空，测试事务状态由Commit和Rollback验证。
func (c *archiveFailureConn) Close() error { return nil }

// Begin 标记当前短事务，供网关边界验证未持事务外呼。
// 参数：无。
// 返回值：当前确定性SQL事务及空错误。
func (c *archiveFailureConn) Begin() (driver.Tx, error) { c.c.inTransaction = true; return c, nil }
func (c *archiveFailureConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}

// Commit 仅把当前事务实际写入的恢复意图与归档状态标记为已提交。
// 参数：无。
// 返回值：空错误；业务状态是否提交由独立字段供断言验证。
func (c *archiveFailureConn) Commit() error {
	c.c.durable = c.c.durable || c.c.intentPending
	c.c.archiveCommitted = c.c.archiveCommitted || c.c.archivePending
	c.c.intentPending = false
	c.c.archivePending = false
	c.c.inTransaction = false
	c.c.commits++
	return nil
}

// Rollback 保留既有持久恢复意图并撤销未提交归档标记。
// 参数：无。
// 返回值：空错误。
func (c *archiveFailureConn) Rollback() error {
	if c.c.intentPending && !c.c.durable {
		c.c.effect = nil
	}
	c.c.intentPending = false
	c.c.archivePending = false
	c.c.inTransaction = false
	return nil
}

// QueryContext 返回新短事务协议需要的业务状态、持久意图及真实领取代次。
// 参数：q为服务实际执行的SQL，其余为当前测试数据库边界。
// 返回值：匹配业务夹具的数据库行，未匹配查询保留不存在语义。
func (c *archiveFailureConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "SELECT operation_token,") {
		return &taxDetailRows{rows: [][]driver.Value{{c.c.operationToken, c.c.operationToken != "", false}}}, nil
	}
	if strings.Contains(q, "FROM settlement_invoice_archive_effect WHERE id=") && c.c.effect != nil {
		return &taxDetailRows{rows: [][]driver.Value{c.c.effect}}, nil
	}
	if strings.Contains(q, "COUNT(*) FROM settlement_tax_invoice") {
		return &taxDetailRows{rows: [][]driver.Value{{int64(1)}}}, nil
	}
	if strings.Contains(q, "SELECT id FROM settlement_tax_invoice") || strings.Contains(q, "SELECT status FROM settlement_tax_invoice") {
		c.c.locks++
		if c.c.onLock != nil {
			c.c.onLock(c.c.locks)
		}
		value := "invoice"
		if strings.Contains(q, "SELECT status") {
			value = "ISSUED"
		}
		return &taxDetailRows{rows: [][]driver.Value{{value}}}, nil
	}
	return &taxDetailRows{}, nil
}

// ExecContext 记录持久恢复意图、操作代次和未提交归档写入，保留原故障注入。
// 参数：q/a为实际服务执行的SQL及绑定参数。
// 返回值：受控影响行数或真实测试注入错误。
func (c *archiveFailureConn) ExecContext(_ context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(q, "SET operation_token=?,operation_until=") {
		c.c.operationToken = a[0].Value.(string)
	}
	if strings.Contains(q, "SET state='ARCHIVED'") {
		c.c.archivePending = true
	}
	if strings.Contains(q, "INSERT INTO settlement_invoice_archive_effect(") {
		c.c.intentPending = true
		c.c.effect = []driver.Value{a[0].Value, a[3].Value, a[4].Value, a[5].Value, a[6].Value, a[7].Value, a[8].Value, a[9].Value, a[10].Value, a[11].Value, a[12].Value, a[13].Value}
		if c.c.onIntent != nil {
			c.c.onIntent()
		}
	}
	if strings.Contains(q, "INSERT INTO settlement_invoice_document(") {
		if c.c.onDocument != nil {
			c.c.onDocument()
		}
		if c.c.failDocument {
			return nil, errors.New("injected document write failure")
		}
	}
	return driver.RowsAffected(1), nil
}

type archiveFailureGateway struct{ c *archiveFailureConnector }

// Upload 执行真实handler调用的网关边界，并验证恢复意图已持久且数据库事务已结束。
// 参数：上下文及上传身份和内容由实际服务提供。
// 返回值：稳定测试文件身份。
func (g archiveFailureGateway) Upload(context.Context, string, string, string, string, string, io.Reader) (string, error) {
	g.c.uploads++
	if g.c.inTransaction {
		g.c.externalWithTransaction = true
	}
	if !g.c.durable {
		g.c.externalWithoutIntent = true
	}
	if g.c.onUpload != nil {
		g.c.onUpload()
	}
	return "file", nil
}

// Bind 记录实际服务绑定调用，并检查持久恢复意图与事务外执行。
// 参数：上下文及精确业务绑定身份由实际服务提供。
// 返回值：空错误。
func (g archiveFailureGateway) Bind(context.Context, string, string, string, string, string, string) error {
	g.c.binds++
	if !g.c.durable {
		g.c.externalWithoutIntent = true
	}
	if g.c.inTransaction {
		g.c.externalWithTransaction = true
	}
	return nil
}
func (g archiveFailureGateway) Download(context.Context, string) ([]byte, error) { return nil, nil }
func (g archiveFailureGateway) RetireUpload(context.Context, string) (string, string, error) {
	return "file", "READY", nil
}
func (g archiveFailureGateway) Release(context.Context, string, string, string, string, string) error {
	return nil
}

// TestInvoiceArchivePersistsRecoveryBeforeGatewaySideEffects 验证真实上传绑定后的归档失败保留持久恢复意图。
// 参数：t为handler、服务与网关完整路径测试上下文。
// 返回值：无，网关未被实际调用、缺少持久意图或持事务外呼时失败。
func TestInvoiceArchivePersistsRecoveryBeforeGatewaySideEffects(t *testing.T) {
	c := &archiveFailureConnector{failDocument: true}
	db := sql.OpenDB(c)
	defer db.Close()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("file", "invoice.pdf")
	_, _ = part.Write([]byte("%PDF-1.4\ninvoice"))
	_ = form.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tax-invoices/invoice/documents", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Idempotency-Key", "key")
	req.Header.Set("Origin", "http://localhost")
	req.Header.Set("X-CSRF-Token", "1")
	response := httptest.NewRecorder()
	New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "http://localhost", OIDCTenantID: "dev", FileGatewayURL: "http://gateway", FileGatewayApplicationID: "settlement"}, nil, nil, nil, nil, archiveFailureGateway{c}, &archiveClockGate{now: time.Unix(100, 0), expires: time.Unix(101, 0)}).ServeHTTP(response, req)
	if response.Code < 400 {
		t.Fatalf("write failure returned %d", response.Code)
	}
	if c.externalWithoutIntent {
		t.Fatal("gateway upload/binding escaped without a committed recovery intent after document transaction failed")
	}
	if c.externalWithTransaction {
		t.Fatal("gateway side effect held a database transaction")
	}
	if !c.durable || c.archiveCommitted || c.binds != 1 {
		t.Fatalf("recovery=%v archived=%v binds=%d", c.durable, c.archiveCommitted, c.binds)
	}
	if c.uploads != 1 {
		t.Fatalf("expected document write failure after a real handler upload; uploads=%d body=%s", c.uploads, response.Body.String())
	}
}

type archiveClockGate struct{ now, expires time.Time }

func (g *archiveClockGate) Check(context.Context, core.Operation) error {
	if !g.now.Before(g.expires) {
		return core.ErrDenied
	}
	return nil
}

// TestInvoiceArchiveRechecksLicenseAcrossWaitAndGatewayBoundaries 验证等待锁、外呼及最终提交各阶段失效均拒绝归档。
// 参数：t为真实handler到服务的许可证边界测试上下文。
// 返回值：无，失效后跨越网关边界、业务提交或丢失既有恢复意图时失败。
func TestInvoiceArchiveRechecksLicenseAcrossWaitAndGatewayBoundaries(t *testing.T) {
	for _, stage := range []string{"missing-gate", "prepare-lock", "intent-write", "execution-lock", "upload", "before-commit"} {
		t.Run(stage, func(t *testing.T) {
			gate := &archiveClockGate{now: time.Unix(100, 0), expires: time.Unix(101, 0)}
			expire := func() { gate.now = gate.expires }
			c := &archiveFailureConnector{}
			switch stage {
			case "prepare-lock":
				c.onLock = func(n int) {
					if n == 1 {
						expire()
					}
				}
			case "intent-write":
				c.onIntent = expire
			case "execution-lock":
				c.onLock = func(n int) {
					if n == 2 {
						expire()
					}
				}
			case "upload":
				c.onUpload = expire
			case "before-commit":
				c.onDocument = expire
			}
			db := sql.OpenDB(c)
			defer db.Close()
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			part, _ := form.CreateFormFile("file", "invoice.pdf")
			_, _ = part.Write([]byte("%PDF-1.7\n%%EOF\n"))
			_ = form.Close()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/tax-invoices/invoice/documents", &body)
			request.Header.Set("Content-Type", form.FormDataContentType())
			request.Header.Set("Idempotency-Key", "key")
			request.Header.Set("Origin", "http://localhost")
			request.Header.Set("X-CSRF-Token", "1")
			response := httptest.NewRecorder()
			var gateDependency any = gate
			if stage == "missing-gate" {
				gateDependency = nil
			}
			New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "http://localhost", OIDCTenantID: "dev", FileGatewayURL: "http://gateway", FileGatewayApplicationID: "settlement"}, nil, nil, nil, nil, archiveFailureGateway{c}, gateDependency).ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "COMMERCIAL_LICENSE_DENIED") {
				t.Fatalf("expired at %s status=%d body=%s uploads=%d binds=%d commits=%d", stage, response.Code, response.Body.String(), c.uploads, c.binds, c.commits)
			}
			wantUploads, wantBinds, wantCommits := 0, 0, 1
			if stage == "missing-gate" || stage == "prepare-lock" || stage == "intent-write" {
				wantCommits = 0
			}
			if stage == "upload" || stage == "before-commit" {
				wantUploads = 1
			}
			if stage == "upload" {
				wantCommits = 2
			}
			if stage == "before-commit" {
				wantBinds = 1
				wantCommits = 3
			}
			if c.archiveCommitted || c.externalWithoutIntent || c.externalWithTransaction {
				t.Fatalf("license boundary violated: archived=%v withoutIntent=%v heldTransaction=%v", c.archiveCommitted, c.externalWithoutIntent, c.externalWithTransaction)
			}
			if c.durable != (wantCommits > 0) {
				t.Fatalf("recovery intent durable=%v commits=%d", c.durable, c.commits)
			}
			if c.uploads != wantUploads || c.binds != wantBinds || c.commits != wantCommits {
				t.Fatalf("expired request crossed boundary: uploads=%d binds=%d commits=%d", c.uploads, c.binds, c.commits)
			}
		})
	}
}
