package service

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type archiveOperationGateway struct {
	mu                    sync.Mutex
	upload, bind, release int
	onUpload, onBind      func()
	failRelease           bool
	entered, proceed      chan struct{}
}

// Upload 提供受控网关边界，允许在真实数据库外呼期间制造并发及状态漂移。
// 参数：ctx及上传身份和内容为实际服务传入的网关请求。
// 返回值：按稳定请求身份派生文件引用，上传阻塞只用于本地并发回归。
func (g *archiveOperationGateway) Upload(ctx context.Context, request, app, purpose, name, media string, content io.Reader) (string, error) {
	g.mu.Lock()
	g.upload++
	hook, entered, proceed := g.onUpload, g.entered, g.proceed
	g.entered = nil
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	if entered != nil {
		close(entered)
		select {
		case <-proceed:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "file-" + request, nil
}

// Bind 提供可注入业务状态改变的同步网关边界。
// 参数：上下文及绑定身份来自真实服务调用。
// 返回值：边界成功，业务是否可提交由服务重新校验。
func (g *archiveOperationGateway) Bind(context.Context, string, string, string, string, string, string) error {
	g.mu.Lock()
	g.bind++
	hook := g.onBind
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

// RetireUpload 精确返回稳定上传请求对应的已退休文件。
// 参数：ctx为恢复请求，request为稳定网关请求身份。
// 返回值：同一上传文件及就绪状态，不生成新文件。
func (g *archiveOperationGateway) RetireUpload(ctx context.Context, request string) (string, string, error) {
	return "file-" + request, "READY", nil
}

// Release 提供第一次释放失败后的幂等恢复边界。
// 参数：上下文及精确业务绑定身份由真实恢复服务提供。
// 返回值：注入失败或空错误，失败不会冒充释放完成。
func (g *archiveOperationGateway) Release(context.Context, string, string, string, string, string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.release++
	if g.failRelease {
		g.failRelease = false
		return ErrDependency
	}
	return nil
}

// archiveAssertRowsUnlocked 在网关回调期间尝试获得真实MySQL业务和意图行锁。
// 参数：t/db为隔离测试库，business/effect为内部固定表，tenant/id为当前业务身份。
// 返回值：无，网关外呼期间仍占用业务或意图行锁时失败。
func archiveAssertRowsUnlocked(t *testing.T, db *sql.DB, business, effect, tenant, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Error(err)
		return
	}
	defer tx.Rollback()
	var found string
	for _, query := range []string{`SELECT id FROM ` + business + ` WHERE tenant_id=? AND id=? FOR UPDATE NOWAIT`, `SELECT id FROM ` + effect + ` WHERE tenant_id=? AND ` + map[string]string{"settlement_tax_invoice": "tax_invoice_id", "settlement_receipt": "receipt_id"}[business] + `=? FOR UPDATE NOWAIT`} {
		if err = tx.QueryRowContext(ctx, query, tenant, id).Scan(&found); err != nil {
			t.Errorf("gateway retained MySQL row lock: %v", err)
		}
	}
}

// TestArchiveOperationMySQLConcurrencyAndInvalidation 验证并发领取、无长事务及绑定期间状态失效恢复。
// 参数：t为明确私有DSN对应的隔离MySQL上下文。
// 返回值：无，重复上传、迟到业务提交或恢复不收敛时失败。
func TestArchiveOperationMySQLConcurrencyAndInvalidation(t *testing.T) {
	db := financeOperationMySQL(t)
	svc := &Service{DB: db}
	ctx := context.Background()
	tenant := "archive-operation-" + newID()
	p := archivePrincipal(tenant)
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "app", GatewayURL: "http://isolated"}
	invoice := seedArchiveInvoice(t, db, tenant)
	input := archiveInput(invoice, "same-key")
	gateway := &archiveOperationGateway{entered: make(chan struct{}), proceed: make(chan struct{})}
	gateway.onUpload = func() {
		archiveAssertRowsUnlocked(t, db, "settlement_tax_invoice", "settlement_invoice_archive_effect", tenant, invoice)
	}
	entered := gateway.entered
	done := make(chan error, 1)
	go func() { _, _, err := svc.UploadInvoiceDocument(ctx, p, identity, gateway, input); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not enter boundary")
	}
	quick, cancel := context.WithTimeout(ctx, time.Second)
	_, _, err := svc.UploadInvoiceDocument(quick, p, identity, gateway, input)
	cancel()
	if !errors.Is(err, ErrConflict) {
		t.Errorf("same-key active upload should conflict promptly: %v", err)
	}
	other := seedArchiveInvoice(t, db, tenant)
	if _, _, err = svc.UploadInvoiceDocument(ctx, p, identity, &archiveOperationGateway{}, archiveInput(other, "other-key")); err != nil {
		t.Error(err)
	}
	close(gateway.proceed)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, created, err := svc.UploadInvoiceDocument(ctx, p, identity, gateway, input); err != nil || created {
		t.Fatalf("READY replay created=%v err=%v", created, err)
	}
	if gateway.upload != 1 {
		t.Fatalf("READY replay repeated upload: %d", gateway.upload)
	}
	for _, phase := range []string{"upload", "bind"} {
		t.Run(phase, func(t *testing.T) {
			invoice := seedArchiveInvoice(t, db, tenant)
			g := &archiveOperationGateway{failRelease: true}
			invalidate := func() {
				if _, err := db.Exec(`UPDATE settlement_tax_invoice SET status='VOID' WHERE tenant_id=? AND id=?`, tenant, invoice); err != nil {
					t.Error(err)
				}
			}
			if phase == "upload" {
				g.onUpload = invalidate
			} else {
				g.onBind = invalidate
			}
			if _, _, err := svc.UploadInvoiceDocument(ctx, p, identity, g, archiveInput(invoice, phase)); !errors.Is(err, ErrConflict) {
				t.Fatalf("invalid business committed: %v", err)
			}
			if phase == "upload" && g.bind != 0 {
				t.Fatal("invalidated business bound late file")
			}
			var documents int
			if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_invoice_document WHERE tenant_id=? AND tax_invoice_id=?`, tenant, invoice).Scan(&documents); err != nil || documents != 0 {
				t.Fatalf("documents=%d err=%v", documents, err)
			}
			dueArchiveEffects(t, db)
			worker := InvoiceArchiveWorker{DB: db, Identity: identity, Gateway: g}
			if found, err := worker.recoverOne(ctx); !found || err == nil {
				t.Fatalf("release failure lost found=%v err=%v", found, err)
			}
			dueArchiveEffects(t, db)
			if found, err := worker.recoverOne(ctx); !found || err != nil {
				t.Fatalf("recovery found=%v err=%v", found, err)
			}
			if archiveState(t, db, invoice) != "RETIRED" {
				t.Fatal("invalid business recovery did not converge")
			}
		})
	}
}

// TestEvidenceOperationMySQLInvalidationRecovery 验证证据绑定期间归属改变及首次释放失败后的同键收敛。
// 参数：t为明确私有DSN对应的隔离MySQL上下文。
// 返回值：无，错误归属READY、丢失补偿意图或重复上传时失败。
func TestEvidenceOperationMySQLInvalidationRecovery(t *testing.T) {
	db := financeOperationMySQL(t)
	tenant := "evidence-operation-" + newID()
	p := recognitionTestPrincipal(tenant, "maker", "settlement_accountant")
	svc := &Service{DB: db, RecognitionDirectory: recognitionTestDirectory{}}
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "app", GatewayURL: "http://isolated"}
	input := RecognitionEvidenceInput{OrganizationID: "org", OrganizationWitnessID: "witness", Name: "evidence.pdf", MediaType: "application/pdf", Content: []byte("%PDF-1.4\noperation evidence")}
	g := &archiveOperationGateway{failRelease: true}
	g.onBind = func() {
		if _, err := db.Exec(`UPDATE settlement_recognition_evidence SET organization_id='other' WHERE tenant_id=?`, tenant); err != nil {
			t.Error(err)
		}
	}
	if _, err := svc.UploadRecognitionEvidence(context.Background(), p, "key", identity, g, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed evidence committed: %v", err)
	}
	var status string
	var recovering bool
	if err := db.QueryRow(`SELECT status,recovering FROM settlement_recognition_evidence WHERE tenant_id=?`, tenant).Scan(&status, &recovering); err != nil || status != "PENDING" || !recovering {
		t.Fatalf("status=%s recovering=%v err=%v", status, recovering, err)
	}
	if _, err := svc.UploadRecognitionEvidence(context.Background(), p, "key", identity, g, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired evidence pretended success: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM settlement_recognition_evidence WHERE tenant_id=?`, tenant).Scan(&status); err != nil || status != "REJECTED" {
		t.Fatalf("recovery status=%s err=%v", status, err)
	}
	if g.upload != 1 || g.release != 2 {
		t.Fatalf("uploads=%d releases=%d", g.upload, g.release)
	}
}

// TestReceiptOperationMySQLInvalidationRecovery 验证回单外呼不持锁及绑定期间失效后的恢复收敛。
// 参数：t为本轮明确授权的私有隔离MySQL上下文。
// 返回值：无，迟到归档、重复上传或首次释放失败后意图丢失时失败。
func TestReceiptOperationMySQLInvalidationRecovery(t *testing.T) {
	db := financeOperationMySQL(t)
	svc := &Service{DB: db}
	ctx := context.Background()
	tenant := "receipt-operation-" + newID()
	p := Principal{TenantID: tenant, UserID: "maker", Permissions: map[string]bool{"settlement.receipt.record": true}}
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "app", GatewayURL: "http://isolated"}
	receipt := seedRefundReceipt(t, db, tenant)
	input := archiveInput(receipt, "receipt-key")
	input.DocumentType = "RECEIPT_IMAGE"
	g := &archiveOperationGateway{failRelease: true}
	g.onUpload = func() {
		archiveAssertRowsUnlocked(t, db, "settlement_receipt", "settlement_receipt_archive_effect", tenant, receipt)
	}
	g.onBind = func() {
		if _, err := db.Exec(`UPDATE settlement_receipt SET status='VOID' WHERE tenant_id=? AND id=?`, tenant, receipt); err != nil {
			t.Error(err)
		}
	}
	if _, _, err := svc.UploadReceiptDocument(ctx, p, identity, g, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalidated receipt committed: %v", err)
	}
	worker := ReceiptArchiveWorker{DB: db, Identity: identity, Gateway: g}
	if found, err := worker.recoverOne(ctx); !found || err == nil {
		t.Fatalf("first release found=%v err=%v", found, err)
	}
	if _, err := db.Exec(`UPDATE settlement_receipt_archive_effect SET available_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND receipt_id=?`, tenant, receipt); err != nil {
		t.Fatal(err)
	}
	if found, err := worker.recoverOne(ctx); !found || err != nil {
		t.Fatalf("retry release found=%v err=%v", found, err)
	}
	var status string
	var count int
	if err := db.QueryRow(`SELECT state FROM settlement_receipt_archive_effect WHERE tenant_id=? AND receipt_id=?`, tenant, receipt).Scan(&status); err != nil || status != "RETIRED" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_receipt_document WHERE tenant_id=? AND receipt_id=?`, tenant, receipt).Scan(&count); err != nil || count != 0 {
		t.Fatalf("document count=%d err=%v", count, err)
	}
}

// TestArchiveOperationMySQLStaleGeneration 验证过期上传被恢复接管后，旧网关响应不能重新归档。
// 参数：t为本轮明确授权的私有隔离MySQL上下文。
// 返回值：无，退休后的旧代次生成文档或绑定时失败。
func TestArchiveOperationMySQLStaleGeneration(t *testing.T) {
	db := financeOperationMySQL(t)
	svc := &Service{DB: db}
	tenant := "stale-operation-" + newID()
	p := archivePrincipal(tenant)
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "app", GatewayURL: "http://isolated"}
	invoice := seedArchiveInvoice(t, db, tenant)
	g := &archiveOperationGateway{}
	g.onUpload = func() {
		if _, err := db.Exec(`UPDATE settlement_invoice_archive_effect SET operation_until=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 SECOND),available_at=UTC_TIMESTAMP(3) WHERE tenant_id=? AND tax_invoice_id=?`, tenant, invoice); err != nil {
			t.Error(err)
		}
		worker := InvoiceArchiveWorker{DB: db, Identity: identity, Gateway: g}
		if found, err := worker.recoverOne(context.Background()); !found || err != nil {
			t.Errorf("recovery takeover found=%v err=%v", found, err)
		}
	}
	if _, _, err := svc.UploadInvoiceDocument(context.Background(), p, identity, g, archiveInput(invoice, "stale")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	if g.bind != 0 {
		t.Fatal("stale upload bound after recovery takeover")
	}
	if state := archiveState(t, db, invoice); state != "RETIRED" {
		t.Fatalf("state=%s", state)
	}
}
