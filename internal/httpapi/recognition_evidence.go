package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/j-s-te/settlement/internal/service"
)

// uploadRecognitionEvidence 通过真实文件网关上传独立财务证据，不接受任意已存在fileID。
// 参数：w/r为受保护请求，p为具备登记及冻结组织范围的主体。
// 返回值：无；只有真实上传绑定与本地提交完成才返回READY证据引用。
func (a *API) uploadRecognitionEvidence(w http.ResponseWriter, r *http.Request, p service.Principal) {
	const maximum = 10 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maximum+(1<<20))
	if r.ParseMultipartForm(maximum) != nil {
		fail(w, 422, "SETTLEMENT_INVALID_EVIDENCE", "财务证据不得超过10MiB")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, e := r.FormFile("file")
	if e != nil {
		fail(w, 422, "SETTLEMENT_INVALID_EVIDENCE", "请选择真实财务证据文件")
		return
	}
	defer file.Close()
	content, e := io.ReadAll(io.LimitReader(file, maximum+1))
	if e != nil || len(content) == 0 || len(content) > maximum {
		fail(w, 422, "SETTLEMENT_INVALID_EVIDENCE", "文件为空或超过10MiB")
		return
	}
	input := service.RecognitionEvidenceInput{OrganizationID: r.FormValue("organization_id"), OrganizationWitnessID: r.FormValue("organization_witness_id"), Name: filepath.Base(header.Filename), MediaType: http.DetectContentType(content), Content: content}
	evidence, e := a.service.UploadRecognitionEvidence(r.Context(), p, r.Header.Get("Idempotency-Key"), service.InvoiceArchiveIdentity{TenantID: a.cfg.OIDCTenantID, ApplicationID: a.cfg.FileGatewayApplicationID, GatewayURL: a.cfg.FileGatewayURL}, a.files, input)
	if e != nil {
		recognitionError(w, e)
		return
	}
	write(w, 201, map[string]any{"id": evidence.ID, "evidence_reference": "file:" + evidence.ID, "status": evidence.Status, "name": evidence.Name, "checksum": evidence.Checksum})
}

// downloadRecognitionEvidence 逐次核对组织ACL、原网关身份与内容摘要后导出财务证据。
// 参数：w/r为受许可门禁保护请求，p为当前主体，id为本地证据ID。
// 返回值：无；文件不存在、无权、来源漂移或审计失败时拒绝下载。
func (a *API) downloadRecognitionEvidence(w http.ResponseWriter, r *http.Request, p service.Principal, id string) {
	evidence, e := a.service.GetRecognitionEvidence(r.Context(), p, id)
	if e != nil {
		recognitionError(w, e)
		return
	}
	if a.files == nil || evidence.ApplicationID != a.cfg.FileGatewayApplicationID || evidence.GatewayOrigin != a.cfg.FileGatewayURL {
		recognitionError(w, service.ErrDependency)
		return
	}
	content, e := a.files.Download(r.Context(), evidence.FileID)
	if e != nil {
		recognitionError(w, service.ErrDependency)
		return
	}
	digest := sha256.Sum256(content)
	if int64(len(content)) != evidence.ByteSize || hex.EncodeToString(digest[:]) != evidence.Checksum {
		recognitionError(w, service.ErrDependency)
		return
	}
	if e = a.service.RecordRecognitionEvidenceDownload(r.Context(), p, id); e != nil {
		recognitionError(w, e)
		return
	}
	w.Header().Set("Content-Type", evidence.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": evidence.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)
	_, _ = w.Write(content)
}

// serveRecognitionEvidence 在通用财务路由之前处理证据的专用上传与下载。
// 参数：w/r为请求响应，p为当前明确财务主体，path为该路由相对路径。
// 返回值：匹配证据操作时为真，未匹配交由通用财务状态机处理。
func (a *API) serveRecognitionEvidence(w http.ResponseWriter, r *http.Request, p service.Principal, path string) bool {
	if path == "/evidence" && r.Method == "POST" {
		a.uploadRecognitionEvidence(w, r, p)
		return true
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 3 && parts[0] == "evidence" && parts[1] != "" && parts[2] == "download" && r.Method == "GET" {
		a.downloadRecognitionEvidence(w, r, p, parts[1])
		return true
	}
	return false
}
