package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type evidenceRevocableDirectory struct {
	invalid     bool
	unavailable bool
}

// RecognitionPeople 模拟证据绑定期间见证人员持续失效的权威目录边界。
// 参数：ctx为查询上下文，id为精确人员身份，keyword不参与测试。
// 返回值：失效前返回原组织见证，失效后持续不存在。
func (d *evidenceRevocableDirectory) RecognitionPeople(ctx context.Context, id, keyword string) ([]RecognitionPerson, error) {
	if d.unavailable {
		return nil, errors.New("synthetic temporary directory failure")
	}
	if d.invalid {
		return nil, nil
	}
	return recognitionTestDirectory{}.RecognitionPeople(ctx, id, keyword)
}

// TestEvidenceDirectoryOutageRetryMySQL 验证绑定后目录短时故障保留同键可恢复意图。
// 参数：t 为显式隔离MySQL测试上下文，不连接业务数据库。
// 返回值：无；短时依赖失败退休文件或恢复后无法用原键完成时失败。
func TestEvidenceDirectoryOutageRetryMySQL(t *testing.T) {
	db := financeOperationMySQL(t)
	tenant := "directory-outage-" + newID()
	p := recognitionTestPrincipal(tenant, "maker", "settlement_accountant")
	directory := &evidenceRevocableDirectory{}
	svc := &Service{DB: db, RecognitionDirectory: directory}
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "app", GatewayURL: "http://isolated"}
	input := RecognitionEvidenceInput{OrganizationID: "org", OrganizationWitnessID: "witness",
		Name: "retry.pdf", MediaType: "application/pdf", Content: []byte("%PDF-1.4\ndirectory retry")}
	gateway := &archiveOperationGateway{onBind: func() { directory.unavailable = true }}
	if _, err := svc.UploadRecognitionEvidence(context.Background(), p, "same-key", identity, gateway, input); !errors.Is(err, ErrDependency) {
		t.Fatalf("directory outage should be retryable: %v", err)
	}
	var status string
	var recovering bool
	if err := db.QueryRow(`SELECT status,recovering FROM settlement_recognition_evidence WHERE tenant_id=?`, tenant).
		Scan(&status, &recovering); err != nil || status != "PENDING" || recovering || gateway.release != 0 {
		t.Fatalf("outage retired durable intent: status=%s recovering=%v releases=%d err=%v", status, recovering, gateway.release, err)
	}
	directory.unavailable = false
	gateway.onBind = nil
	actual, err := svc.UploadRecognitionEvidence(context.Background(), p, "same-key", identity, gateway, input)
	if err != nil || actual.Status != "READY" || gateway.release != 0 {
		t.Fatalf("same key did not recover: %+v %v releases=%d", actual, err, gateway.release)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_recognition_evidence WHERE tenant_id=?`, tenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate evidence created: count=%d err=%v", count, err)
	}
}

// TestEvidenceInvalidWitnessCleanupRetryMySQL 验证见证持续失效且首次释放失败后同键仍可清理旧意图。
// 参数：t为本轮明确授权的真实隔离MySQL上下文。
// 返回值：无，目录失效导致补偿永久卡住或重试重复上传时失败。
func TestEvidenceInvalidWitnessCleanupRetryMySQL(t *testing.T) {
	db := financeOperationMySQL(t)
	tenant := "witness-recovery-" + newID()
	p := recognitionTestPrincipal(tenant, "maker", "settlement_accountant")
	directory := &evidenceRevocableDirectory{}
	svc := &Service{DB: db, RecognitionDirectory: directory}
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "app", GatewayURL: "http://isolated"}
	input := RecognitionEvidenceInput{OrganizationID: "org", OrganizationWitnessID: "witness", Name: "recovery.pdf", MediaType: "application/pdf", Content: []byte("%PDF-1.4\nwitness recovery")}
	gateway := &archiveOperationGateway{failRelease: true, onBind: func() { directory.invalid = true }}
	if _, err := svc.UploadRecognitionEvidence(context.Background(), p, "key", identity, gateway, input); err == nil {
		t.Fatal("invalid witness pretended READY")
	}
	if gateway.release != 1 {
		t.Fatal("first compensation not attempted")
	}
	_, _ = svc.UploadRecognitionEvidence(context.Background(), p, "key", identity, gateway, input)
	var status string
	if err := db.QueryRow(`SELECT status FROM settlement_recognition_evidence WHERE tenant_id=?`, tenant).Scan(&status); err != nil || status != "REJECTED" {
		t.Fatalf("persistent invalid witness stranded cleanup: status=%s err=%v", status, err)
	}
	if gateway.upload != 1 || gateway.release != 2 {
		t.Fatalf("upload=%d release=%d", gateway.upload, gateway.release)
	}
}

// TestEvidenceBackgroundRecoveryAfterWitnessRevocationMySQL 验证目录持续失效且人员撤权后后台无需重试上传即可清理。
// 参数：t为本轮明确授权的真实隔离MySQL上下文。
// 返回值：无，后台扩大清理范围、绕过许可证或重复上传时失败。
func TestEvidenceBackgroundRecoveryAfterWitnessRevocationMySQL(t *testing.T) {
	db := financeOperationMySQL(t)
	tenant := "background-evidence-" + newID()
	ctx := context.Background()
	maker := recognitionTestPrincipal(tenant, "maker", "settlement_accountant")
	directory := &evidenceRevocableDirectory{}
	svc := &Service{DB: db, RecognitionDirectory: directory}
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "app", GatewayURL: "http://isolated"}
	input := RecognitionEvidenceInput{OrganizationID: "org", OrganizationWitnessID: "witness", Name: "background.pdf", MediaType: "application/pdf", Content: []byte("%PDF-1.4\nbackground recovery")}
	gateway := &archiveOperationGateway{failRelease: true, onBind: func() { directory.invalid = true }}
	if _, err := svc.UploadRecognitionEvidence(ctx, maker, "key", identity, gateway, input); err == nil {
		t.Fatal("invalid witness accepted")
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM settlement_recognition_evidence WHERE tenant_id=?`, tenant).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// 同一数据库中保留非recovering的PENDING和READY，后台必须精确跳过。
	for _, row := range []struct {
		id, status string
		recovering bool
	}{{newID(), "PENDING", false}, {newID(), "READY", true}} {
		_, err := db.Exec(`INSERT INTO settlement_recognition_evidence(id,tenant_id,organization_id,created_by,name,media_type,checksum,byte_size,status,application_id,gateway_origin,file_id,created_at,updated_at,recovering) SELECT ?,tenant_id,organization_id,created_by,name,media_type,checksum,byte_size,?,application_id,gateway_origin,file_id,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),? FROM settlement_recognition_evidence WHERE tenant_id=? AND id=?`, row.id, row.status, row.recovering, tenant, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	revoked := maker
	revoked.Permissions = nil
	revoked.Roles = nil
	revoked.DataScopes = nil
	if _, err := svc.UploadRecognitionEvidence(ctx, revoked, "key", identity, gateway, input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked user bypassed access: %v", err)
	}
	if _, err := svc.UploadRecognitionEvidence(ctx, maker, "new-invalid-witness", identity, gateway, input); !errors.Is(err, ErrInvalid) {
		t.Fatalf("new upload bypassed directory: %v", err)
	}
	altered := input
	altered.OrganizationWitnessID = "different-witness"
	if _, err := svc.UploadRecognitionEvidence(ctx, maker, "key", identity, gateway, altered); !errors.Is(err, ErrConflict) {
		t.Fatalf("different witness obtained cleanup: %v", err)
	}
	otherActor := maker
	otherActor.UserID = "other-maker"
	if _, err := svc.UploadRecognitionEvidence(ctx, otherActor, "key", identity, gateway, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("different actor obtained cleanup: %v", err)
	}
	changedOrigin := identity
	changedOrigin.GatewayURL = "http://other-origin"
	if _, err := svc.UploadRecognitionEvidence(ctx, maker, "key", changedOrigin, gateway, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("different gateway obtained cleanup: %v", err)
	}
	if gateway.release != 1 || gateway.upload != 1 {
		t.Fatal("invalid retry changed external state")
	}
	worker := RecognitionEvidenceRecoveryWorker{DB: db, Identity: identity, Gateway: gateway}
	denied := errors.New("isolated license denied")
	worker.CheckBusinessLicense = func(context.Context) error { return denied }
	if err := worker.RunOnce(ctx); !errors.Is(err, denied) {
		t.Fatalf("license denied ignored: %v", err)
	}
	worker.CheckBusinessLicense = nil
	worker.Identity = changedOrigin
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if gateway.release != 1 {
		t.Fatal("wrong gateway released original binding")
	}
	worker.Identity = identity
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM settlement_recognition_evidence WHERE tenant_id=? AND id=?`, tenant, id).Scan(&status); err != nil || status != "REJECTED" {
		t.Fatalf("background cleanup status=%s err=%v", status, err)
	}
	if gateway.upload != 1 || gateway.bind != 1 || gateway.release != 2 {
		t.Fatalf("upload=%d bind=%d release=%d", gateway.upload, gateway.bind, gateway.release)
	}
	var intents, unrelated int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_idempotency_record WHERE tenant_id=? AND command_type='RECOGNITION_EVIDENCE_UPLOAD'`, tenant).Scan(&intents); err != nil || intents != 1 {
		t.Fatalf("invalid upload created new intent count=%d err=%v", intents, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settlement_recognition_evidence WHERE tenant_id=? AND id<>? AND status IN ('READY','PENDING')`, tenant, id).Scan(&unrelated); err != nil || unrelated != 2 {
		t.Fatalf("unrelated cleanup count=%d err=%v", unrelated, err)
	}
}

type evidenceRecoveryFairGateway struct {
	archiveOperationGateway
	stage    string
	failures map[string]bool
	attempts map[string]int
}

// RetireUpload 对指定旧意图持续制造退休失败，其它请求保持稳定文件身份。
// 参数：ctx为清理上下文，request为原持久上传请求身份。
// 返回值：指定故障或稳定退休结果，不创建新文件。
func (g *evidenceRecoveryFairGateway) RetireUpload(ctx context.Context, request string) (string, string, error) {
	id := strings.TrimPrefix(request, "recognition-evidence-")
	if g.stage == "retire" {
		g.attempts[id]++
		if g.failures[id] {
			return "", "", ErrDependency
		}
	}
	return g.archiveOperationGateway.RetireUpload(ctx, request)
}

// Release 对指定旧意图持续制造释放失败，不阻断其它意图的精确释放。
// 参数：ctx/app/file/resource/id/binding为原持久文件及业务绑定身份。
// 返回值：指定故障或原网关边界的释放结果。
func (g *evidenceRecoveryFairGateway) Release(ctx context.Context, app, file, resource, id, binding string) error {
	if g.stage == "release" {
		g.attempts[id]++
		if g.failures[id] {
			return ErrDependency
		}
	}
	return g.archiveOperationGateway.Release(ctx, app, file, resource, id, binding)
}

// TestEvidenceRecoveryFailureDoesNotStarveNextBatchMySQL 验证最老20条持续失败时第21条在下一批仍可清理。
// 参数：t为本轮明确授权的真实隔离MySQL上下文。
// 返回值：无，失败意图反复占满批次、热重试或阻断后续意图时失败。
func TestEvidenceRecoveryFailureDoesNotStarveNextBatchMySQL(t *testing.T) {
	db := financeOperationMySQL(t)
	for _, stage := range []string{"retire", "release"} {
		t.Run(stage, func(t *testing.T) {
			tenant := "fair-evidence-" + newID()
			past := time.Now().UTC().Add(-time.Hour)
			gateway := &evidenceRecoveryFairGateway{stage: stage, failures: map[string]bool{}, attempts: map[string]int{}}
			var last string
			for index := 0; index < 21; index++ {
				id := newID()
				last = id
				if index < 20 {
					gateway.failures[id] = true
				}
				at := past.Add(time.Duration(index) * time.Millisecond)
				_, err := db.Exec(`INSERT INTO settlement_recognition_evidence(id,tenant_id,organization_id,created_by,name,media_type,checksum,byte_size,status,application_id,gateway_origin,file_id,created_at,updated_at,recovering) VALUES(?,?,'org','maker','fair.pdf','application/pdf',?,1,'PENDING','app','http://isolated',?,?,?,TRUE)`, id, tenant, strings.Repeat("a", 64), "file-recognition-evidence-"+id, at, at)
				if err != nil {
					t.Fatal(err)
				}
			}
			worker := RecognitionEvidenceRecoveryWorker{DB: db, Identity: InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "app", GatewayURL: "http://isolated"}, Gateway: gateway}
			if err := worker.RunOnce(context.Background()); err == nil {
				t.Fatal("persistent failures were hidden")
			}
			for id := range gateway.failures {
				if gateway.attempts[id] != 1 {
					t.Fatalf("intent %s attempted %d times in one batch", id, gateway.attempts[id])
				}
			}
			if gateway.attempts[last] != 0 {
				t.Fatal("first batch exceeded twenty intent limit")
			}
			if err := worker.RunOnce(context.Background()); err == nil {
				t.Fatal("persistent older failures were hidden")
			}
			var status string
			if err := db.QueryRow(`SELECT status FROM settlement_recognition_evidence WHERE tenant_id=? AND id=?`, tenant, last).Scan(&status); err != nil || status != "REJECTED" {
				t.Fatalf("healthy twenty-first intent starved: status=%s err=%v", status, err)
			}
			if gateway.release != 1 || gateway.upload != 0 || gateway.bind != 0 {
				t.Fatalf("release=%d upload=%d bind=%d", gateway.release, gateway.upload, gateway.bind)
			}
		})
	}
}
