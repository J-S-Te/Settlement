package service

import (
	"context"
	"errors"
	"io"
	"testing"
)

type recognitionEvidenceGateway struct {
	uploadCalls, bindCalls int
	bindFails              bool
}

// Upload 模拟文件网关的幂等上传边界，实际证据状态与审计使用真实隔离MySQL。
// 参数：ctx/request/app/purpose/name/media/content为完整上传身份及内容。
// 返回值：稳定文件引用；不访问业务环境。
func (g *recognitionEvidenceGateway) Upload(ctx context.Context, request, app, purpose, name, media string, content io.Reader) (string, error) {
	g.uploadCalls++
	if purpose != "RECOGNITION_EVIDENCE" {
		return "", ErrInvalid
	}
	return "gateway-file", nil
}

// Bind 模拟已上传文件的绑定失败与重试恢复。
// 参数：ctx/app/file/resource/id/binding/name为明确的绑定身份。
// 返回值：故障注入时错误，恢复后成功。
func (g *recognitionEvidenceGateway) Bind(ctx context.Context, app, file, resource, id, binding, name string) error {
	g.bindCalls++
	if resource != "settlement_recognition_evidence" {
		return ErrInvalid
	}
	if g.bindFails {
		return errors.New("isolated bind failure")
	}
	return nil
}

// RetireUpload 满足既有网关接口，本门禁不退休或删除上传。
// 参数：ctx/id为待观察的上传身份。
// 返回值：明确未使用的边界错误。
func (g *recognitionEvidenceGateway) RetireUpload(ctx context.Context, id string) (string, string, error) {
	return "", "", ErrDependency
}

// Release 满足既有网关接口，本门禁不释放他人文件。
// 参数：ctx/app/file/resource/id/binding为绑定身份。
// 返回值：明确未使用的边界错误。
func (g *recognitionEvidenceGateway) Release(ctx context.Context, app, file, resource, id, binding string) error {
	return ErrDependency
}

// TestRecognitionEvidenceMySQLRetryAndScope 验证真实证据意图、绑定失败恢复及跨组织下载拒绝。
// 参数：t为测试上下文，只使用当前私有隔离DSN。
// 返回值：无；虚假READY、不同内容重放或越权下载失败即报错。
func TestRecognitionEvidenceMySQLRetryAndScope(t *testing.T) {
	db := dashboardFinanceMySQL(t)
	tenant := "evidence-" + newID()
	p := recognitionTestPrincipal(tenant, "maker", "settlement_accountant")
	svc := &Service{DB: db, RecognitionDirectory: recognitionTestDirectory{}}
	gateway := &recognitionEvidenceGateway{bindFails: true}
	identity := InvoiceArchiveIdentity{TenantID: tenant, ApplicationID: "settlement-app", GatewayURL: "http://isolated-files"}
	input := RecognitionEvidenceInput{OrganizationID: "org", OrganizationWitnessID: "witness", Name: "依据.pdf", MediaType: "application/pdf", Content: []byte("%PDF-1.4\nisolated evidence")}
	if _, e := svc.UploadRecognitionEvidence(context.Background(), p, "evidence-key", identity, gateway, input); e == nil {
		t.Fatal("bind failure pretended success")
	}
	var state string
	if e := db.QueryRow(`SELECT status FROM settlement_recognition_evidence WHERE tenant_id=?`, tenant).Scan(&state); e != nil || state != "PENDING" {
		t.Fatalf("state=%s err=%v", state, e)
	}
	gateway.bindFails = false
	evidence, e := svc.UploadRecognitionEvidence(context.Background(), p, "evidence-key", identity, gateway, input)
	if e != nil || evidence.Status != "READY" || gateway.uploadCalls != 2 {
		t.Fatalf("recovery=%+v err=%v", evidence, e)
	}
	replay, e := svc.UploadRecognitionEvidence(context.Background(), p, "evidence-key", identity, gateway, input)
	if e != nil || replay.ID != evidence.ID || gateway.uploadCalls != 2 {
		t.Fatalf("replay=%+v err=%v", replay, e)
	}
	draftInput := recognitionTestInput("file:"+evidence.ID, "evidence-fact", "INCOME", 100)
	draft, e := svc.CreateRecognition(context.Background(), p, "file-evidence-draft", draftInput)
	if e != nil || draft.EvidenceReference != "file:"+evidence.ID {
		t.Fatalf("ready evidence not usable: %+v %v", draft, e)
	}
	input.Content = []byte("%PDF-1.4\nchanged")
	if _, e = svc.UploadRecognitionEvidence(context.Background(), p, "evidence-key", identity, gateway, input); !errors.Is(e, ErrConflict) {
		t.Fatalf("changed content=%v", e)
	}
	narrow := p
	narrow.DataScopes = []DataScope{{RoleCode: "settlement_accountant", ScopeType: "ORG", ScopeID: "other"}}
	if _, e = svc.GetRecognitionEvidence(context.Background(), narrow, evidence.ID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross organization=%v", e)
	}
	other := p
	other.TenantID += "other"
	if _, e = svc.GetRecognitionEvidence(context.Background(), other, evidence.ID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross tenant=%v", e)
	}
}
