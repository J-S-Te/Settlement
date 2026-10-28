package httpapi

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/j-s-te/settlement/internal/service"
	"io"
	"mime"
	"net/http"
	"strings"
)

// serveRoleFinance 分发设置、加签及财务工作流路由。
// 参数：w 响应，r 已经过商业许可及CSRF入口的请求。
// 返回值：路由是否被本模块处理。
func (a *API) serveRoleFinance(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	switch {
	case path == "settings":
		a.roleSettings(w, r)
	case strings.HasPrefix(path, "invoice-requests/") && (strings.HasSuffix(path, "/special-approve") || strings.HasSuffix(path, "/special-reject")):
		a.roleSpecialReview(w, r)
	case path == "receipt-differences" || strings.HasPrefix(path, "receipt-differences/"):
		a.roleDifferences(w, r)
	case path == "accounting-vouchers" || strings.HasPrefix(path, "accounting-vouchers/"):
		a.roleVouchers(w, r)
	case strings.HasPrefix(path, "receipts/") && strings.Contains(path, "/documents"):
		a.roleReceiptDocuments(w, r)
	default:
		return false
	}
	return true
}

// roleSettings 读取或保存受控配置。
// 参数：w 响应，r 请求。
// 返回值：无。
func (a *API) roleSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.settings.manage")
	if !ok {
		return
	}
	var in service.SettlementSettings
	var err error
	switch r.Method {
	case http.MethodGet:
		in, err = a.service.GetSettlementSettings(r.Context(), p)
	case http.MethodPut:
		if !decode(w, r, &in) {
			return
		}
		in, err = a.service.PutSettlementSettings(r.Context(), p, in)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, in)
}

// roleSpecialReview 执行总监加签动作。
// 参数：w 响应，r 请求。
// 返回值：无。
func (a *API) roleSpecialReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	p, ok := a.user(w, r, "settlement.invoice.special_approve")
	if !ok {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-requests/"), "/")
	if len(parts) != 2 {
		respondError(w, service.ErrInvalid)
		return
	}
	var in struct {
		Version int    `json:"version"`
		Reason  string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	attempt, err := a.service.ReviewSpecialInvoice(r.Context(), p, parts[0], in.Version, parts[1] == "special-approve", in.Reason)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]string{"attempt_id": attempt})
}

// roleDifferences 处理差异暂挂、处置与独立复核。
// 参数：w 响应，r 请求。
// 返回值：无。
func (a *API) roleDifferences(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/receipt-differences"), "/")
	permission := "settlement.difference.manage"
	if len(parts) == 3 && parts[2] == "approve" {
		permission = "settlement.difference.approve"
	}
	p, ok := a.user(w, r, permission)
	if !ok {
		return
	}
	var err error
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			var items []map[string]any
			query, valid := roleFinanceQuery(w, r)
			if !valid {
				return
			}
			items, err = a.service.ListFinanceRecords(r.Context(), p, false, query)
			if err == nil {
				write(w, http.StatusOK, map[string]any{"items": items})
				return
			}
		case http.MethodPost:
			var in service.ReceiptDifferenceInput
			if !decode(w, r, &in) {
				return
			}
			var id string
			in.Key = r.Header.Get("Idempotency-Key")
			id, err = a.service.CreateReceiptDifference(r.Context(), p, in)
			if err == nil {
				write(w, http.StatusCreated, map[string]string{"id": id})
				return
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
	} else if len(parts) == 3 && r.Method == http.MethodPost {
		if parts[2] == "confirm-refund" {
			a.roleConfirmRefund(w, r, p, parts[1])
			return
		}
		var in service.DifferenceResolutionInput
		if !decode(w, r, &in) {
			return
		}
		switch parts[2] {
		case "resolve":
			err = a.service.ResolveReceiptDifference(r.Context(), p, parts[1], in)
		case "approve":
			err = a.service.ApproveReceiptDifference(r.Context(), p, parts[1], in.Version)
		default:
			err = service.ErrNotFound
		}
		if err == nil {
			write(w, http.StatusOK, map[string]string{"status": "OK"})
			return
		}
	} else {
		err = service.ErrNotFound
	}
	respondError(w, err)
}

// roleVouchers 生成和重试本地财务凭证。
// 参数：w 响应，r 请求。
// 返回值：无。
func (a *API) roleVouchers(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.voucher.manage")
	if !ok {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/accounting-vouchers"), "/")
	var err error
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			var items []map[string]any
			query, valid := roleFinanceQuery(w, r)
			if !valid {
				return
			}
			items, err = a.service.ListFinanceRecords(r.Context(), p, true, query)
			if err == nil {
				write(w, http.StatusOK, map[string]any{"items": items})
				return
			}
		case http.MethodPost:
			var in struct {
				WriteoffID string `json:"writeoff_id"`
			}
			if !decode(w, r, &in) {
				return
			}
			var id, status string
			id, err = a.service.CreateAccountingVoucher(r.Context(), p, in.WriteoffID)
			if err == nil {
				status, err = a.service.RetryAccountingVoucher(r.Context(), p, id)
			}
			if err == nil {
				write(w, http.StatusCreated, map[string]string{"id": id, "status": status})
				return
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
	} else if len(parts) == 3 && parts[2] == "retry" && r.Method == http.MethodPost {
		var status string
		status, err = a.service.RetryAccountingVoucher(r.Context(), p, parts[1])
		if err == nil {
			write(w, http.StatusOK, map[string]string{"status": status})
			return
		}
	} else {
		err = service.ErrNotFound
	}
	respondError(w, err)
}

// roleReceiptDocuments 校验独立回单权限后分发影像操作。
// 参数：w 响应，r 请求。
// 返回值：无。
func (a *API) roleReceiptDocuments(w http.ResponseWriter, r *http.Request) {
	permission := "settlement.receipt.read"
	if r.Method == http.MethodPost {
		permission = "settlement.receipt.record"
	}
	p, ok := a.user(w, r, permission)
	if !ok {
		return
	}
	if !p.Unrestricted(permission) {
		respondError(w, service.ErrForbidden)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/receipts/"), "/")
	if len(parts) < 2 || parts[1] != "documents" {
		respondError(w, service.ErrNotFound)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost {
		a.uploadRoleReceipt(w, r, p, parts[0])
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if len(parts) == 4 && parts[3] == "download" {
		a.downloadRoleReceipt(w, r, p, parts[0], parts[2])
		return
	}
	if len(parts) != 2 {
		respondError(w, service.ErrNotFound)
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT id,original_name,media_type FROM settlement_receipt_document WHERE tenant_id=? AND receipt_id=? ORDER BY created_at,id LIMIT 200`, p.TenantID, parts[0])
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]string{}
	for rows.Next() {
		var id, name, media string
		if err = rows.Scan(&id, &name, &media); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]string{"id": id, "original_name": name, "media_type": media, "status": "ARCHIVED"})
	}
	if err = rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"items": items})
}

// uploadRoleReceipt 解析有界影像并交给持久化归档流程。
// 参数：w 响应，r 请求，p 主体，receipt 回款标识。
// 返回值：无。
func (a *API) uploadRoleReceipt(w http.ResponseWriter, r *http.Request, p service.Principal, receipt string) {
	var in struct {
		Filename      string `json:"filename"`
		ContentType   string `json:"content_type"`
		ContentBase64 string `json:"content_base64"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 15<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		respondError(w, service.ErrInvalid)
		return
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		respondError(w, service.ErrInvalid)
		return
	}
	content, err := base64.StdEncoding.DecodeString(in.ContentBase64)
	if err != nil {
		respondError(w, service.ErrInvalid)
		return
	}
	id, created, err := a.service.UploadReceiptDocument(r.Context(), p, service.InvoiceArchiveIdentity{TenantID: a.cfg.OIDCTenantID, ApplicationID: a.cfg.FileGatewayApplicationID, GatewayURL: a.cfg.FileGatewayURL}, a.files, service.InvoiceArchiveInput{Key: r.Header.Get("Idempotency-Key"), InvoiceID: receipt, DocumentType: "RECEIPT_IMAGE", OriginalName: in.Filename, MediaType: in.ContentType, Checksum: fmt.Sprintf("%x", sha256.Sum256(content)), Content: content})
	if err != nil {
		respondError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	write(w, status, map[string]string{"id": id, "status": "ARCHIVED"})
}

// downloadRoleReceipt 从网关读取实际回单内容并记录审计。
// 参数：w 响应，r 请求，p 主体，receipt 回款标识，id 文档标识。
// 返回值：无。
func (a *API) downloadRoleReceipt(w http.ResponseWriter, r *http.Request, p service.Principal, receipt, id string) {
	if a.files == nil {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_FILE_GATEWAY_UNAVAILABLE", "文件网关未配置")
		return
	}
	var file, name, media string
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT storage_object_id,original_name,media_type FROM settlement_receipt_document WHERE tenant_id=? AND receipt_id=? AND id=?`, p.TenantID, receipt, id).Scan(&file, &name, &media)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = service.ErrNotFound
		}
		respondError(w, err)
		return
	}
	content, err := a.files.Download(r.Context(), file)
	if err != nil {
		fail(w, http.StatusBadGateway, "SETTLEMENT_FILE_DOWNLOAD_FAILED", "回单下载失败")
		return
	}
	if err = a.service.AuditAction(r.Context(), p, "SETTLEMENT_RECEIPT_DOCUMENT_DOWNLOADED", "receipt_document", id, map[string]any{"receipt_id": receipt}); err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// roleFinanceQuery 复用统一分页边界并提取财务列表筛选。
// 参数：w 响应，r 列表请求。
// 返回值：查询条件及是否通过分页格式校验。
func roleFinanceQuery(w http.ResponseWriter, r *http.Request) (service.FinanceRecordQuery, bool) {
	limit, offset, err := page(r)
	if err != nil {
		respondError(w, service.ErrInvalid)
		return service.FinanceRecordQuery{}, false
	}
	return service.FinanceRecordQuery{Limit: limit, Offset: offset, Status: r.URL.Query().Get("status"), Keyword: r.URL.Query().Get("keyword")}, true
}

// roleConfirmRefund 解码外部完成事实并执行受控确认。
// 参数：w 响应，r 请求，p 可信财务主体，id 差异标识。
// 返回值：无。
func (a *API) roleConfirmRefund(w http.ResponseWriter, r *http.Request, p service.Principal, id string) {
	var in service.ConfirmRefundRequest
	if !decode(w, r, &in) {
		return
	}
	if err := a.service.ConfirmReceiptRefund(r.Context(), p, id, in); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]string{"status": "OK"})
}
