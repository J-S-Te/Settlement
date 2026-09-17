package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
)

type API struct {
	service    *service.Service
	cfg        config.Config
	logger     *slog.Logger
	auth       *platform.Authenticator
	machine    *platform.ServiceTokenVerifier
	taxMachine *platform.ServiceTokenVerifier
	directory  platform.PersonnelDirectory
	files      InvoiceFileGateway
}

type InvoiceFileGateway interface {
	Upload(context.Context, string, string, string, string, string, io.Reader) (string, error)
	Bind(context.Context, string, string, string, string, string, string) error
	Download(context.Context, string) ([]byte, error)
}

func New(db *sql.DB, cfg config.Config, logger *slog.Logger, auth *platform.Authenticator, machine, taxMachine *platform.ServiceTokenVerifier, dependencies ...any) http.Handler {
	var directory platform.PersonnelDirectory
	var publisher service.CreditEventPublisher
	var files InvoiceFileGateway
	for _, dependency := range dependencies {
		if value, ok := dependency.(platform.PersonnelDirectory); ok {
			directory = value
		}
		if value, ok := dependency.(service.CreditEventPublisher); ok {
			publisher = value
		}
		if value, ok := dependency.(InvoiceFileGateway); ok {
			files = value
		}
	}
	return &API{service: &service.Service{DB: db, CreditPublisher: publisher, InvoiceIssuanceMode: cfg.InvoiceIssuanceMode, TaxProviderCode: cfg.TaxProviderCode}, cfg: cfg, logger: logger, auth: auth, machine: machine, taxMachine: taxMachine, directory: directory, files: files}
}
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if requestID == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		requestID = hex.EncodeToString(b)
	}
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("X-CSRF-Token") != "1" || strings.TrimRight(r.Header.Get("Origin"), "/") != strings.TrimRight(a.cfg.PublicOrigin, "/") {
			fail(w, http.StatusForbidden, "SETTLEMENT_CSRF_REJECTED", "写请求来源校验失败")
			return
		}
	}
	switch {
	case r.URL.Path == "/healthz":
		write(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	case r.URL.Path == "/readyz":
		if err := a.service.DB.PingContext(r.Context()); err != nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_NOT_READY", "数据库不可用")
			return
		}
		write(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	case r.URL.Path == "/auth/login":
		a.login(w, r)
		return
	case r.URL.Path == "/auth/callback":
		if a.auth == nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_OIDC_UNCONFIGURED", "Settlement 身份服务尚未配置")
		} else {
			a.auth.Callback(w, r)
		}
		return
	case r.URL.Path == "/auth/logout":
		if a.auth == nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_OIDC_UNCONFIGURED", "Settlement 身份服务尚未配置")
		} else {
			a.auth.Logout(w, r)
		}
		return
	case r.URL.Path == "/auth/local-logout" && r.Method == http.MethodPost:
		if a.auth == nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_OIDC_UNCONFIGURED", "Settlement 身份服务尚未配置")
		} else {
			a.auth.LogoutLocal(w, r)
		}
		return
	case r.URL.Path == "/auth/backchannel-logout" && r.Method == http.MethodPost:
		if a.auth == nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_OIDC_UNCONFIGURED", "Settlement 身份服务尚未配置")
		} else {
			a.auth.BackchannelLogout(w, r)
		}
		return
	case r.URL.Path == "/auth/me" || r.URL.Path == "/api/v1/auth/me":
		a.me(w, r)
		return
	case r.URL.Path == "/logged-out":
		write(w, http.StatusOK, map[string]string{"status": "logged_out"})
		return
	case r.URL.Path == "/internal/v1/settlement/events/contracts" && r.Method == http.MethodPost:
		a.contractEvent(w, r)
		return
	case r.URL.Path == "/internal/v1/settlement/events/tax-results" && r.Method == http.MethodPost:
		a.taxResult(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/credit-sync-events/") && strings.HasSuffix(r.URL.Path, "/retry") && r.Method == http.MethodPost:
		a.retryCreditSync(w, r)
		return
	case r.URL.Path == "/api/v1/dashboard" && r.Method == http.MethodGet:
		a.dashboard(w, r)
		return
	case r.URL.Path == "/api/v1/reports/invoiced-receivables-top10" && r.Method == http.MethodGet:
		a.invoicedReceivablesTop10(w, r)
		return
	case r.URL.Path == "/api/v1/reports/invoiced-receivables/export" && r.Method == http.MethodPost:
		a.createInvoicedReceivablesExport(w, r)
		return
	case r.URL.Path == "/api/v1/reports/aging/export" && r.Method == http.MethodPost:
		a.createAgingReceivablesExport(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/reports/exports/") && strings.HasSuffix(r.URL.Path, "/download") && r.Method == http.MethodGet:
		a.downloadReportExport(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/reports/exports/") && r.Method == http.MethodGet:
		a.reportExport(w, r)
		return
	case r.URL.Path == "/api/v1/receivable-plans" && r.Method == http.MethodGet:
		a.listPlans(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/receivable-plans/") && strings.HasSuffix(r.URL.Path, "/confirm") && r.Method == http.MethodPost:
		a.confirmPlan(w, r)
		return
	case r.URL.Path == "/api/v1/receivables" && r.Method == http.MethodGet:
		a.listReceivables(w, r, false)
		return
	case r.URL.Path == "/api/v1/invoice-eligible-receivables" && r.Method == http.MethodGet:
		// 开票选择器只读取结算库快照，避免浏览器直接访问合同管理数据库。
		a.listReceivables(w, r, true)
		return
	case r.URL.Path == "/api/v1/receipts" && r.Method == http.MethodGet:
		a.listReceipts(w, r)
		return
	case r.URL.Path == "/api/v1/receipts" && r.Method == http.MethodPost:
		a.createReceipt(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/receipts/") && strings.HasSuffix(r.URL.Path, "/matches") && r.Method == http.MethodGet:
		a.receiptMatches(w, r)
		return
	case r.URL.Path == "/api/v1/receipt-allocations" && r.Method == http.MethodPost:
		a.createAllocation(w, r)
		return
	case r.URL.Path == "/api/v1/receipt-allocations" && r.Method == http.MethodGet:
		a.listAllocations(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/receipt-allocations/") && strings.HasSuffix(r.URL.Path, "/reverse") && r.Method == http.MethodPost:
		a.reverseAllocation(w, r)
		return
	case r.URL.Path == "/api/v1/invoice-requests" && r.Method == http.MethodGet:
		a.listInvoiceRequests(w, r)
		return
	case r.URL.Path == "/api/v1/tax-invoices" && r.Method == http.MethodGet:
		a.listTaxInvoices(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/tax-invoices/") && strings.HasSuffix(r.URL.Path, "/documents") && r.Method == http.MethodPost:
		a.uploadInvoiceDocument(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/tax-invoices/") && strings.HasSuffix(r.URL.Path, "/download") && r.Method == http.MethodGet:
		a.downloadInvoiceDocument(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/tax-invoices/") && strings.HasSuffix(r.URL.Path, "/red-flush") && r.Method == http.MethodPost:
		a.requestInvoiceRedFlush(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-red-flush-requests/") && strings.HasSuffix(r.URL.Path, "/approve") && r.Method == http.MethodPost:
		a.reviewInvoiceRedFlush(w, r, true)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-red-flush-requests/") && strings.HasSuffix(r.URL.Path, "/reject") && r.Method == http.MethodPost:
		a.reviewInvoiceRedFlush(w, r, false)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/tax-invoices/") && r.Method == http.MethodGet:
		a.taxInvoiceDetail(w, r)
		return
	case r.URL.Path == "/api/v1/invoice-requests" && r.Method == http.MethodPost:
		a.createInvoiceRequest(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-requests/") && strings.HasSuffix(r.URL.Path, "/approve") && r.Method == http.MethodPost:
		a.approveInvoiceRequest(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-requests/") && strings.HasSuffix(r.URL.Path, "/reject") && r.Method == http.MethodPost:
		a.rejectInvoiceRequest(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-requests/") && strings.HasSuffix(r.URL.Path, "/manual-issue") && r.Method == http.MethodPost:
		a.manualIssueInvoice(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/policies" && r.Method == http.MethodGet:
		a.listDunningPolicies(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/policies" && r.Method == http.MethodPost:
		a.createDunningPolicy(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/cases" && r.Method == http.MethodGet:
		a.listDunningCases(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/actions" && r.Method == http.MethodGet:
		a.listDunningActions(w, r)
		return
	case r.URL.Path == "/api/v1/dunning/recipients" && r.Method == http.MethodGet:
		a.listDunningRecipients(w, r)
		return
	case r.URL.Path == "/api/v1/notifications" && r.Method == http.MethodGet:
		a.listNotifications(w, r)
		return
	case r.URL.Path == "/api/v1/notifications/unread-count" && r.Method == http.MethodGet:
		a.unreadCount(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/notifications/") && strings.HasSuffix(r.URL.Path, "/read") && r.Method == http.MethodPost:
		a.readNotification(w, r)
		return
	default:
		fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到接口")
	}
}

func (a *API) retryCreditSync(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.credit.resend")
	if !ok {
		return
	}
	eventID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/credit-sync-events/"), "/retry")
	if eventID == "" || strings.Contains(eventID, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "信用同步事件标识不合法")
		return
	}
	if err := a.service.RetryCreditSync(r.Context(), p.TenantID, eventID); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到信用同步事件")
			return
		}
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_CREDIT_SYNC_FAILED", "CRM 信用同步失败，已保留待重发状态")
		return
	}
	write(w, http.StatusOK, map[string]string{"event_id": eventID, "status": "DELIVERED"})
}

// Development auth is deliberately explicit and local only. Production must
// install the same OIDC session adapter/client-credentials verifier used by the
// other subsystems before it exposes any business route.
func (a *API) principal(r *http.Request) (service.Principal, error) {
	if !a.cfg.DevelopmentAuth {
		if a.auth == nil {
			return service.Principal{}, errors.New("OIDC adapter is not configured")
		}
		return a.auth.Authenticate(r.Context(), r)
	}
	return service.Principal{TenantID: "dev", UserID: "dev-finance", IdentityID: "dev-finance", DisplayName: "本地结算管理员", Username: "dev-finance", CatalogVersion: "development", AuthorizationRevision: 1, Permissions: map[string]bool{"settlement.admin": true}}, nil
}
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	if a.cfg.DevelopmentAuth {
		http.Redirect(w, r, "/settlement/dashboard", http.StatusFound)
		return
	}
	if a.auth == nil {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_OIDC_UNCONFIGURED", "Settlement OIDC 会话适配器尚未配置")
		return
	}
	a.auth.Login(w, r)
}
func (a *API) me(w http.ResponseWriter, r *http.Request) {
	p, err := a.principal(r)
	if err != nil {
		if errors.Is(err, platform.ErrUnauthenticated) {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_UNAUTHENTICATED", "登录已失效，请重新登录")
		} else {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_IDENTITY_UNAVAILABLE", "身份服务暂不可用")
		}
		return
	}
	permissions := make([]string, 0, len(p.Permissions))
	for permission, allowed := range p.Permissions {
		if allowed {
			permissions = append(permissions, permission)
		}
	}
	write(w, http.StatusOK, map[string]any{
		"tenant_id": p.TenantID, "user_id": p.UserID, "identity_id": p.IdentityID,
		"person_id": p.PersonID, "name": p.DisplayName, "preferred_username": p.Username,
		"permissions": permissions, "catalog_version": p.CatalogVersion,
		"authorization_revision": p.AuthorizationRevision,
	})
}
func (a *API) integration(r *http.Request) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !a.cfg.IntegrationEnabled || token == "" {
		return false
	}
	if a.cfg.DevelopmentAuth {
		return subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.IntegrationBearerToken)) == 1
	}
	if a.machine == nil {
		return false
	}
	_, err := a.machine.Verify(r.Context(), token)
	return err == nil
}
func (a *API) contractEvent(w http.ResponseWriter, r *http.Request) {
	if !a.integration(r) {
		fail(w, http.StatusUnauthorized, "SETTLEMENT_MACHINE_UNAUTHENTICATED", "服务间令牌无效")
		return
	}
	var event service.ContractEvent
	if !decode(w, r, &event) {
		return
	}
	created, err := a.service.IngestContract(r.Context(), "contract_management", event)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusAccepted, map[string]any{"event_id": event.EventID, "accepted": true, "duplicate": !created})
}
func (a *API) taxResult(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !a.cfg.TaxResultIngestEnabled || token == "" {
		fail(w, http.StatusUnauthorized, "SETTLEMENT_TAX_MACHINE_UNAUTHENTICATED", "税控结果服务令牌无效")
		return
	}
	trustedTenant := a.cfg.OIDCTenantID
	if a.cfg.DevelopmentAuth {
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.TaxResultBearerToken)) != 1 {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_TAX_MACHINE_UNAUTHENTICATED", "税控结果服务令牌无效")
			return
		}
	} else {
		if a.taxMachine == nil {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_TAX_MACHINE_UNAUTHENTICATED", "税控结果服务令牌无效")
			return
		}
		identity, err := a.taxMachine.Verify(r.Context(), token)
		if err != nil {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_TAX_MACHINE_UNAUTHENTICATED", "税控结果服务令牌无效")
			return
		}
		trustedTenant = identity.TenantID
	}
	var event service.TaxCallbackEvent
	if !decode(w, r, &event) {
		return
	}
	if trustedTenant == "" {
		trustedTenant = event.TenantID
	}
	result, err := a.service.ApplyTaxCallback(r.Context(), trustedTenant, event)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusAccepted, map[string]any{"event_id": event.EventID, "accepted": true, "processing_status": result, "duplicate": result != "APPLIED"})
}
func (a *API) dashboard(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.report.read")
	if !ok {
		return
	}
	var receivables, overdue, plans, invoices int
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*),COALESCE(SUM(CASE WHEN due_date<CURDATE() AND open_amount>0 THEN 1 ELSE 0 END),0) FROM settlement_receivable WHERE tenant_id=?`, p.TenantID).Scan(&receivables, &overdue); err != nil {
		respondError(w, err)
		return
	}
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM settlement_receivable_plan WHERE tenant_id=? AND confirmation_status='PENDING_CONFIRMATION'`, p.TenantID).Scan(&plans); err != nil {
		respondError(w, err)
		return
	}
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM settlement_invoice_request WHERE tenant_id=? AND status IN ('SUBMITTED','UNDER_REVIEW')`, p.TenantID).Scan(&invoices); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]int{"receivable_count": receivables, "overdue_count": overdue, "pending_plan_count": plans, "pending_invoice_count": invoices})
}
func (a *API) invoicedReceivablesTop10(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.report.read")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT r.id,r.receivable_no,cs.source_contract_no,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.open_amount,r.currency,r.collection_status,r.invoice_status FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id WHERE r.tenant_id=? AND r.recognition_status='CONFIRMED' AND r.open_amount>0 AND r.invoice_status<>'NOT_INVOICED' ORDER BY r.open_amount DESC,r.due_date ASC,r.id ASC LIMIT 10`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, contract, customer, due, open, currency, collection, invoice string
		if err := rows.Scan(&id, &no, &contract, &customer, &due, &open, &currency, &collection, &invoice); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"receivable_id": id, "receivable_no": no, "contract_no": contract, "customer_name": customer, "due_date": due, "open_amount": open, "currency": currency, "collection_status": collection, "invoice_status": invoice})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"as_of_date": time.Now().UTC().Format("2006-01-02"), "items": items})
}
func (a *API) createInvoicedReceivablesExport(w http.ResponseWriter, r *http.Request) {
	a.createReportExport(w, r, "INVOICED_RECEIVABLES_CSV")
}
func (a *API) createAgingReceivablesExport(w http.ResponseWriter, r *http.Request) {
	a.createReportExport(w, r, "AGING_RECEIVABLES_CSV")
}
func (a *API) createReportExport(w http.ResponseWriter, r *http.Request, reportType string) {
	p, ok := a.user(w, r, "settlement.report.export")
	if !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "需要有效的 Idempotency-Key")
		return
	}
	var id, existingType string
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT id,report_type FROM settlement_report_export_job WHERE tenant_id=? AND requested_by=? AND idempotency_key=?`, p.TenantID, p.UserID, key).Scan(&id, &existingType)
	if err == nil && existingType != reportType {
		fail(w, http.StatusConflict, "SETTLEMENT_IDEMPOTENCY_CONFLICT", "幂等键已用于其他导出任务")
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		id = newRequestID()
		_, err = a.service.DB.ExecContext(r.Context(), `INSERT INTO settlement_report_export_job(id,tenant_id,requested_by,report_type,idempotency_key,status,created_at,updated_at) VALUES(?,?,?,?,?,'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, p.UserID, reportType, key)
	}
	if err != nil {
		respondError(w, err)
		return
	}
	a.writeReportExport(w, r, p, id, http.StatusAccepted)
}
func (a *API) reportExport(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.report.export")
	if !ok {
		return
	}
	a.writeReportExport(w, r, p, strings.TrimPrefix(r.URL.Path, "/api/v1/reports/exports/"), http.StatusOK)
}
func (a *API) writeReportExport(w http.ResponseWriter, r *http.Request, p service.Principal, id string, statusCode int) {
	var reportType, status, fileName, errorMessage string
	var createdAt, completedAt, expiresAt sql.NullTime
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT report_type,status,file_name,error_message,created_at,completed_at,expires_at FROM settlement_report_export_job WHERE id=? AND tenant_id=? AND requested_by=?`, id, p.TenantID, p.UserID).Scan(&reportType, &status, &fileName, &errorMessage, &createdAt, &completedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到导出任务")
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	item := map[string]any{"id": id, "report_type": reportType, "status": status, "file_name": fileName, "error_message": errorMessage, "created_at": createdAt.Time.Format(time.RFC3339), "completed_at": "", "expires_at": ""}
	if completedAt.Valid {
		item["completed_at"] = completedAt.Time.Format(time.RFC3339)
	}
	if expiresAt.Valid {
		item["expires_at"] = expiresAt.Time.Format(time.RFC3339)
	}
	write(w, statusCode, item)
}
func (a *API) downloadReportExport(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.report.export")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/reports/exports/"), "/download")
	var status, filename, contentType string
	var content []byte
	var expiresAt sql.NullTime
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT status,file_name,content_type,file_content,expires_at FROM settlement_report_export_job WHERE id=? AND tenant_id=? AND requested_by=?`, id, p.TenantID, p.UserID).Scan(&status, &filename, &contentType, &content, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到导出任务")
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	if expiresAt.Valid && expiresAt.Time.Before(time.Now().UTC()) {
		fail(w, http.StatusGone, "SETTLEMENT_EXPORT_EXPIRED", "导出文件已过期，请重新导出")
		return
	}
	if status != "READY" || len(content) == 0 {
		fail(w, http.StatusConflict, "SETTLEMENT_EXPORT_NOT_READY", "导出文件尚未生成完成")
		return
	}
	if err = a.service.AuditAction(r.Context(), p, "SETTLEMENT_REPORT_EXPORT_DOWNLOADED", "report_export", id, map[string]any{"report_type": "AGING_OR_INVOICED", "risk_level": "HIGH"}); err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(filename, `"`, "")+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}
func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}
func (a *API) listPlans(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receivable.read")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT rp.id,cs.source_contract_no,cs.customer_name_snapshot,rp.installment_no,DATE_FORMAT(rp.due_date,'%Y-%m-%d'),rp.planned_amount,rp.confirmation_status,rp.version FROM settlement_receivable_plan rp JOIN settlement_contract_snapshot cs ON cs.id=rp.contract_snapshot_id AND cs.tenant_id=rp.tenant_id WHERE rp.tenant_id=? ORDER BY rp.due_date ASC LIMIT 100`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, customer, due, planned, status string
		var installment, version int
		if err := rows.Scan(&id, &no, &customer, &installment, &due, &planned, &status, &version); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "contract_no": no, "customer_name": customer, "installment_no": installment, "due_date": due, "planned_amount": planned, "status": status, "version": version})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) confirmPlan(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receivable.confirm")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/receivable-plans/"), "/confirm")
	var body service.PlanConfirmationInput
	if !decode(w, r, &body) {
		return
	}
	if err := a.service.ConfirmPlan(r.Context(), p, id, body); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

func (a *API) receiptMatches(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.allocation.confirm")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/receipts/"), "/matches")
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT r.id,r.receivable_no,cs.source_contract_no,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.open_amount,r.currency,LEAST(r.open_amount,rc.unallocated_amount),CASE WHEN r.open_amount=rc.unallocated_amount THEN 100 WHEN r.open_amount>rc.unallocated_amount THEN 80 ELSE 70 END FROM settlement_receipt rc JOIN settlement_receivable r ON r.tenant_id=rc.tenant_id AND r.currency=rc.currency JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id AND cs.customer_id=rc.customer_id WHERE rc.id=? AND rc.tenant_id=? AND rc.unallocated_amount>0 AND r.recognition_status='CONFIRMED' AND r.open_amount>0 ORDER BY 9 DESC,r.due_date ASC,r.id ASC LIMIT 20`, id, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var receivableID, no, contract, customer, due, open, currency, suggested string
		var score int
		if err := rows.Scan(&receivableID, &no, &contract, &customer, &due, &open, &currency, &suggested, &score); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"receivable_id": receivableID, "receivable_no": no, "contract_no": contract, "customer_name": customer, "due_date": due, "open_amount": open, "currency": currency, "suggested_amount": suggested, "score": score})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) listReceivables(w http.ResponseWriter, r *http.Request, invoiceEligibleOnly bool) {
	p, ok := a.user(w, r, "settlement.receivable.read")
	if !ok {
		return
	}
	limit, offset, err := page(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
		return
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	where := []string{"r.tenant_id = ?"}
	args := []any{p.TenantID}
	if invoiceEligibleOnly {
		where = append(where, "cs.financial_status = 'EFFECTIVE'", "r.recognition_status = 'CONFIRMED'", "r.original_amount-r.invoiced_amount-COALESCE((SELECT SUM(a.reserved_amount-a.invoiced_amount-a.released_amount) FROM settlement_invoice_request_allocation a WHERE a.tenant_id=r.tenant_id AND a.receivable_id=r.id AND a.status='RESERVED'),0) > 0")
	}
	if keyword != "" {
		where = append(where, "(r.receivable_no LIKE ? OR cs.source_contract_no LIKE ? OR cs.customer_name_snapshot LIKE ?)")
		like := "%" + keyword + "%"
		args = append(args, like, like, like)
	}
	query := `SELECT r.id,r.receivable_no,r.contract_snapshot_id,cs.source_contract_no,cs.customer_id,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.original_amount,r.open_amount,r.currency,r.collection_status,r.invoice_status,r.version, r.original_amount-r.invoiced_amount-COALESCE((SELECT SUM(a.reserved_amount-a.invoiced_amount-a.released_amount) FROM settlement_invoice_request_allocation a WHERE a.tenant_id=r.tenant_id AND a.receivable_id=r.id AND a.status='RESERVED'),0) FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id WHERE ` + strings.Join(where, " AND ") + ` ORDER BY r.due_date ASC,r.id ASC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := a.service.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, snapshot, contract, customerID, customer, due, original, open, currency, collection, invoice, invoiceable string
		var version int
		if err := rows.Scan(&id, &no, &snapshot, &contract, &customerID, &customer, &due, &original, &open, &currency, &collection, &invoice, &version, &invoiceable); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "receivable_no": no, "contract_snapshot_id": snapshot, "contract_no": contract, "customer_id": customerID, "customer_name": customer, "due_date": due, "original_amount": original, "open_amount": open, "invoiceable_amount": invoiceable, "currency": currency, "collection_status": collection, "invoice_status": invoice, "version": version})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) listReceipts(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receipt.record")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT id,receipt_no,customer_name_snapshot,amount,unallocated_amount,currency,DATE_FORMAT(receipt_date,'%Y-%m-%d'),payment_method,bank_transaction_reference,status FROM settlement_receipt WHERE tenant_id=? ORDER BY receipt_date DESC LIMIT 100`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, customer, amount, unallocated, currency, date, method, reference, status string
		if err := rows.Scan(&id, &no, &customer, &amount, &unallocated, &currency, &date, &method, &reference, &status); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "receipt_no": no, "customer_name": customer, "amount": amount, "unallocated_amount": unallocated, "currency": currency, "receipt_date": date, "payment_method": method, "bank_transaction_reference": reference, "status": status})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

// page parses bounded list pagination so a client cannot request an unbounded
// financial result set. The default remains compatible with the first-phase UI.
func page(r *http.Request) (int, int, error) {
	limit := 100
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return 0, 0, fmt.Errorf("limit 必须是 1 到 100 之间的整数")
		}
		limit = value
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > 100000 {
			return 0, 0, fmt.Errorf("offset 必须是 0 到 100000 之间的整数")
		}
		offset = value
	}
	return limit, offset, nil
}

func (a *API) createReceipt(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receipt.record")
	if !ok {
		return
	}
	var in service.ReceiptInput
	if !decode(w, r, &in) {
		return
	}
	id, err := a.service.CreateReceipt(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": id})
}
func (a *API) createAllocation(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.allocation.confirm")
	if !ok {
		return
	}
	var in service.AllocationInput
	if !decode(w, r, &in) {
		return
	}
	id, err := a.service.ConfirmAllocation(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": id, "status": "CONFIRMED"})
}
func (a *API) listAllocations(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.allocation.confirm")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT a.id,a.allocation_no,rc.receipt_no,rv.receivable_no,a.allocated_amount,a.currency,a.status,a.match_mode,a.match_confidence,a.confirmed_by,a.confirmed_at FROM settlement_receipt_allocation a JOIN settlement_receipt rc ON rc.id=a.receipt_id AND rc.tenant_id=a.tenant_id JOIN settlement_receivable rv ON rv.id=a.receivable_id AND rv.tenant_id=a.tenant_id WHERE a.tenant_id=? ORDER BY a.confirmed_at DESC LIMIT 100`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, receipt, receivable, amount, currency, status, matchMode, by string
		var matchConfidence int
		var at any
		if err := rows.Scan(&id, &no, &receipt, &receivable, &amount, &currency, &status, &matchMode, &matchConfidence, &by, &at); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "allocation_no": no, "receipt_no": receipt, "receivable_no": receivable, "amount": amount, "currency": currency, "status": status, "match_mode": matchMode, "match_confidence": matchConfidence, "confirmed_by": by, "confirmed_at": at})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) reverseAllocation(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.allocation.reverse")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/receipt-allocations/"), "/reverse")
	var in service.ReversalInput
	if !decode(w, r, &in) {
		return
	}
	result, err := a.service.ReverseAllocation(r.Context(), p, r.Header.Get("Idempotency-Key"), id, in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": result, "status": "REVERSED"})
}
func (a *API) createInvoiceRequest(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.request")
	if !ok {
		return
	}
	var in service.InvoiceRequestInput
	if !decode(w, r, &in) {
		return
	}
	id, err := a.service.CreateInvoiceRequest(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": id, "status": "SUBMITTED"})
}
func (a *API) approveInvoiceRequest(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.approve")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-requests/"), "/approve")
	var in struct {
		Version int `json:"version"`
	}
	if !decode(w, r, &in) {
		return
	}
	attempt, err := a.service.ApproveInvoiceRequest(r.Context(), p, id, in.Version)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusAccepted, map[string]string{"id": id, "attempt_id": attempt, "status": "ISSUE_PENDING"})
}
func (a *API) rejectInvoiceRequest(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.approve")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-requests/"), "/reject")
	var in struct {
		Version int    `json:"version"`
		Reason  string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := a.service.RejectInvoiceRequest(r.Context(), p, id, in.Version, in.Reason); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]string{"id": id, "status": "REJECTED"})
}
func (a *API) manualIssueInvoice(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.issue")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-requests/"), "/manual-issue")
	var in service.ManualInvoiceInput
	if !decode(w, r, &in) {
		return
	}
	invoiceID, err := a.service.RegisterManualInvoice(r.Context(), p, id, in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": invoiceID, "status": "ISSUED"})
}
func (a *API) listInvoiceRequests(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.read")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT ir.id,ir.request_no,cs.source_contract_no,ir.buyer_profile_snapshot,ir.invoice_type,ir.amount_excl_tax,ir.tax_amount,ir.amount_incl_tax,ir.status,ir.submitted_by,ir.approved_by,ir.rejection_reason,ir.version,ir.created_at,(SELECT COUNT(DISTINCT ii.tax_rate) FROM settlement_invoice_request_item ii WHERE ii.tenant_id=ir.tenant_id AND ii.invoice_request_id=ir.id),(SELECT MIN(ii.tax_rate) FROM settlement_invoice_request_item ii WHERE ii.tenant_id=ir.tenant_id AND ii.invoice_request_id=ir.id),COALESCE((SELECT ia.channel FROM settlement_invoice_issue_attempt ia WHERE ia.tenant_id=ir.tenant_id AND ia.invoice_request_id=ir.id ORDER BY ia.attempt_no DESC,ia.requested_at DESC LIMIT 1),'') FROM settlement_invoice_request ir JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id WHERE ir.tenant_id=? ORDER BY ir.created_at DESC LIMIT 100`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, contract, kind, excl, tax, incl, status, submitted, approved, rejectionReason, issueChannel string
		var buyer []byte
		var taxRate sql.NullString
		var taxRateCount int
		var version int
		var created any
		if err := rows.Scan(&id, &no, &contract, &buyer, &kind, &excl, &tax, &incl, &status, &submitted, &approved, &rejectionReason, &version, &created, &taxRateCount, &taxRate, &issueChannel); err != nil {
			respondError(w, err)
			return
		}
		buyerName, buyerTaxNo := buyerSummary(buyer)
		items = append(items, map[string]any{"id": id, "request_no": no, "contract_no": contract, "buyer_name": buyerName, "buyer_tax_no_masked": maskTaxNo(buyerTaxNo), "invoice_type": kind, "amount_excl_tax": excl, "tax_amount": tax, "amount_incl_tax": incl, "tax_rate_display": taxRateDisplay(taxRate.String, taxRateCount), "status": status, "submitted_by": submitted, "approved_by": approved, "rejection_reason": rejectionReason, "version": version, "created_at": created, "issue_channel": issueChannel})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (a *API) listTaxInvoices(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.read")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT ti.id,ti.invoice_request_id,ti.invoice_code,ti.invoice_no,cs.source_contract_no,ti.buyer_profile_snapshot,ti.invoice_type,ti.amount_incl_tax,DATE_FORMAT(ti.issue_date,'%Y-%m-%d'),ti.status,ti.issued_by_channel,COALESCE((SELECT rf.status FROM settlement_invoice_red_flush_request rf WHERE rf.tenant_id=ti.tenant_id AND rf.original_invoice_id=ti.id ORDER BY rf.created_at DESC LIMIT 1),'') FROM settlement_tax_invoice ti JOIN settlement_invoice_request ir ON ir.id=ti.invoice_request_id AND ir.tenant_id=ti.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id WHERE ti.tenant_id=? ORDER BY ti.issue_date DESC,ti.created_at DESC LIMIT 100`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, requestID, code, number, contract, kind, amount, issueDate, status, channel, redFlushStatus string
		var buyer []byte
		if err := rows.Scan(&id, &requestID, &code, &number, &contract, &buyer, &kind, &amount, &issueDate, &status, &channel, &redFlushStatus); err != nil {
			respondError(w, err)
			return
		}
		buyerName, buyerTaxNo := buyerSummary(buyer)
		items = append(items, map[string]any{"id": id, "invoice_request_id": requestID, "invoice_code": code, "invoice_no": number, "contract_no": contract, "buyer_name": buyerName, "buyer_tax_no_masked": maskTaxNo(buyerTaxNo), "invoice_type": kind, "amount_incl_tax": amount, "issue_date": issueDate, "status": status, "issued_by_channel": channel, "red_flush_status": redFlushStatus})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (a *API) taxInvoiceDetail(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.read")
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/tax-invoices/")
	if id == "" || strings.Contains(id, "/") {
		fail(w, http.StatusNotFound, "TAX_INVOICE_NOT_FOUND", "发票不存在")
		return
	}
	var invoiceID, requestID, requestNo, code, number, contract, kind, excl, tax, incl, issueDate, status, channel string
	var buyer, seller []byte
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT ti.id,ti.invoice_request_id,ir.request_no,ti.invoice_code,ti.invoice_no,cs.source_contract_no,ti.buyer_profile_snapshot,ti.seller_profile_snapshot,ti.invoice_type,ti.amount_excl_tax,ti.tax_amount,ti.amount_incl_tax,DATE_FORMAT(ti.issue_date,'%Y-%m-%d'),ti.status,ti.issued_by_channel FROM settlement_tax_invoice ti JOIN settlement_invoice_request ir ON ir.id=ti.invoice_request_id AND ir.tenant_id=ti.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id AND cs.tenant_id=ir.tenant_id WHERE ti.id=? AND ti.tenant_id=?`, id, p.TenantID).Scan(&invoiceID, &requestID, &requestNo, &code, &number, &contract, &buyer, &seller, &kind, &excl, &tax, &incl, &issueDate, &status, &channel)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "TAX_INVOICE_NOT_FOUND", "发票不存在")
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	buyerName, buyerTaxNo := buyerSummary(buyer)
	sellerName, _ := buyerSummary(seller)
	itemRows, err := a.service.DB.QueryContext(r.Context(), `SELECT line_no,item_name,tax_classification_code,amount_excl_tax,tax_rate,tax_amount,amount_incl_tax FROM settlement_invoice_request_item WHERE tenant_id=? AND invoice_request_id=? ORDER BY line_no`, p.TenantID, requestID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer itemRows.Close()
	items := []map[string]any{}
	for itemRows.Next() {
		var line int
		var name, classification, itemExcl, rate, itemTax, itemIncl string
		if err := itemRows.Scan(&line, &name, &classification, &itemExcl, &rate, &itemTax, &itemIncl); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"line_no": line, "item_name": name, "tax_classification_code": classification, "amount_excl_tax": itemExcl, "tax_rate_display": taxRateDisplay(rate, 1), "tax_amount": itemTax, "amount_incl_tax": itemIncl})
	}
	if err := itemRows.Err(); err != nil {
		respondError(w, err)
		return
	}
	documentRows, err := a.service.DB.QueryContext(r.Context(), `SELECT id,document_type,original_name,media_type,archived_at FROM settlement_invoice_document WHERE tenant_id=? AND tax_invoice_id=? ORDER BY archived_at DESC`, p.TenantID, invoiceID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer documentRows.Close()
	documents := []map[string]any{}
	for documentRows.Next() {
		var documentID, documentType, originalName, mediaType string
		var archivedAt any
		if err := documentRows.Scan(&documentID, &documentType, &originalName, &mediaType, &archivedAt); err != nil {
			respondError(w, err)
			return
		}
		documents = append(documents, map[string]any{"id": documentID, "document_type": documentType, "original_name": originalName, "media_type": mediaType, "archived_at": archivedAt})
	}
	if err := documentRows.Err(); err != nil {
		respondError(w, err)
		return
	}
	var redFlushID, redFlushStatus, redFlushReason, redFlushRequestedBy, redFlushReviewReason string
	var redFlushVersion int
	redErr := a.service.DB.QueryRowContext(r.Context(), `SELECT id,status,reason_detail,requested_by,review_reason,version FROM settlement_invoice_red_flush_request WHERE tenant_id=? AND original_invoice_id=? ORDER BY created_at DESC LIMIT 1`, p.TenantID, invoiceID).Scan(&redFlushID, &redFlushStatus, &redFlushReason, &redFlushRequestedBy, &redFlushReviewReason, &redFlushVersion)
	if redErr != nil && !errors.Is(redErr, sql.ErrNoRows) {
		respondError(w, redErr)
		return
	}
	var redFlush any
	if redErr == nil {
		redFlush = map[string]any{"id": redFlushID, "status": redFlushStatus, "reason_detail": redFlushReason, "review_reason": redFlushReviewReason, "version": redFlushVersion, "can_review": redFlushStatus == "SUBMITTED" && redFlushRequestedBy != p.UserID && p.Allows("settlement.invoice.approve")}
	}
	write(w, http.StatusOK, map[string]any{"id": invoiceID, "invoice_request_id": requestID, "request_no": requestNo, "invoice_code": code, "invoice_no": number, "contract_no": contract, "buyer_name": buyerName, "buyer_tax_no_masked": maskTaxNo(buyerTaxNo), "seller_name": sellerName, "invoice_type": kind, "amount_excl_tax": excl, "tax_amount": tax, "amount_incl_tax": incl, "issue_date": issueDate, "status": status, "issued_by_channel": channel, "document_count": len(documents), "documents": documents, "red_flush_request": redFlush, "items": items})
}

func (a *API) uploadInvoiceDocument(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.issue")
	if !ok {
		return
	}
	if a.files == nil || a.cfg.FileGatewayApplicationID == "" {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_FILE_GATEWAY_UNAVAILABLE", "电子发票文件服务尚未配置")
		return
	}
	invoiceID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/tax-invoices/"), "/documents")
	if invoiceID == "" || strings.Contains(invoiceID, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "发票标识不合法")
		return
	}
	var invoiceExists int
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM settlement_tax_invoice WHERE id=? AND tenant_id=?`, invoiceID, p.TenantID).Scan(&invoiceExists); err != nil {
		respondError(w, err)
		return
	}
	if invoiceExists == 0 {
		fail(w, http.StatusNotFound, "SETTLEMENT_TAX_INVOICE_NOT_FOUND", "发票不存在")
		return
	}
	const maxInvoiceDocumentBytes = 10 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxInvoiceDocumentBytes+(1<<20))
	if err := r.ParseMultipartForm(maxInvoiceDocumentBytes); err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "电子发票文件不得超过 10 MiB")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "请选择电子发票文件")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxInvoiceDocumentBytes+1))
	if err != nil || len(content) == 0 || len(content) > maxInvoiceDocumentBytes {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "电子发票文件为空或超过 10 MiB")
		return
	}
	originalName := filepath.Base(strings.TrimSpace(header.Filename))
	extension := strings.ToLower(filepath.Ext(originalName))
	allowedExtensions := map[string]bool{".pdf": true, ".ofd": true, ".xml": true, ".png": true, ".jpg": true, ".jpeg": true}
	if originalName == "." || originalName == "" || len(originalName) > 255 || !allowedExtensions[extension] {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "仅支持 PDF、OFD、XML、PNG 或 JPG 电子发票文件")
		return
	}
	detectedType, _, _ := mime.ParseMediaType(http.DetectContentType(content))
	mediaTypeByExtension := map[string]string{".pdf": "application/pdf", ".ofd": "application/ofd", ".xml": "application/xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg"}
	validContent := map[string]map[string]bool{
		".pdf":  {"application/pdf": true},
		".ofd":  {"application/zip": true},
		".xml":  {"application/xml": true, "text/xml": true, "text/plain": true},
		".png":  {"image/png": true},
		".jpg":  {"image/jpeg": true},
		".jpeg": {"image/jpeg": true},
	}
	if !validContent[extension][detectedType] {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "电子发票扩展名与文件内容不一致")
		return
	}
	mediaType := mediaTypeByExtension[extension]
	documentType := strings.TrimSpace(r.FormValue("document_type"))
	if documentType == "" {
		documentType = "ELECTRONIC_INVOICE"
	}
	if documentType != "ELECTRONIC_INVOICE" && documentType != "INVOICE_IMAGE" {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_DOCUMENT", "电子发票文件类型不合法")
		return
	}
	checksum := fmt.Sprintf("%x", sha256.Sum256(content))
	var existingID string
	err = a.service.DB.QueryRowContext(r.Context(), `SELECT id FROM settlement_invoice_document WHERE tenant_id=? AND tax_invoice_id=? AND document_type=? AND checksum=?`, p.TenantID, invoiceID, documentType, checksum).Scan(&existingID)
	if err == nil {
		write(w, http.StatusOK, map[string]string{"id": existingID, "status": "ARCHIVED"})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		respondError(w, err)
		return
	}
	requestID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if requestID == "" {
		fail(w, http.StatusBadRequest, "SETTLEMENT_IDEMPOTENCY_KEY_REQUIRED", "上传电子发票需要幂等键")
		return
	}
	objectID, err := a.files.Upload(r.Context(), requestID, a.cfg.FileGatewayApplicationID, "INTERNAL", originalName, mediaType, bytes.NewReader(content))
	if err != nil {
		fail(w, http.StatusBadGateway, "SETTLEMENT_FILE_UPLOAD_FAILED", "电子发票上传失败，请稍后重试")
		return
	}
	if err = a.files.Bind(r.Context(), a.cfg.FileGatewayApplicationID, objectID, "tax_invoice", invoiceID, "electronic_invoice", originalName); err != nil {
		fail(w, http.StatusBadGateway, "SETTLEMENT_FILE_BIND_FAILED", "电子发票归档失败，请稍后重试")
		return
	}
	documentID, err := a.service.ArchiveInvoiceDocument(r.Context(), p, invoiceID, documentType, originalName, mediaType, objectID, checksum)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": documentID, "status": "ARCHIVED"})
}

func (a *API) downloadInvoiceDocument(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.read")
	if !ok {
		return
	}
	if a.files == nil {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_FILE_GATEWAY_UNAVAILABLE", "电子发票文件服务尚未配置")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/tax-invoices/")
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[1] != "documents" || parts[3] != "download" {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "电子发票文件标识不合法")
		return
	}
	var objectID, originalName, mediaType string
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT storage_object_id,original_name,media_type FROM settlement_invoice_document WHERE tenant_id=? AND tax_invoice_id=? AND id=?`, p.TenantID, parts[0], parts[2]).Scan(&objectID, &originalName, &mediaType)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "SETTLEMENT_INVOICE_DOCUMENT_NOT_FOUND", "电子发票文件不存在")
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	content, err := a.files.Download(r.Context(), objectID)
	if err != nil {
		fail(w, http.StatusBadGateway, "SETTLEMENT_FILE_DOWNLOAD_FAILED", "电子发票下载失败，请稍后重试")
		return
	}
	if err = a.service.AuditAction(r.Context(), p, "SETTLEMENT_INVOICE_DOCUMENT_DOWNLOADED", "invoice_document", parts[2], map[string]any{"invoice_id": parts[0], "risk_level": "HIGH"}); err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": originalName}))
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (a *API) requestInvoiceRedFlush(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.issue")
	if !ok {
		return
	}
	invoiceID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/tax-invoices/"), "/red-flush")
	if invoiceID == "" || strings.Contains(invoiceID, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "发票标识不合法")
		return
	}
	var in service.InvoiceRedFlushInput
	if !decode(w, r, &in) {
		return
	}
	id, err := a.service.RequestInvoiceRedFlush(r.Context(), p, r.Header.Get("Idempotency-Key"), invoiceID, in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusAccepted, map[string]string{"id": id, "status": "SUBMITTED"})
}

func (a *API) reviewInvoiceRedFlush(w http.ResponseWriter, r *http.Request, approve bool) {
	p, ok := a.user(w, r, "settlement.invoice.approve")
	if !ok {
		return
	}
	suffix := "/reject"
	if approve {
		suffix = "/approve"
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/invoice-red-flush-requests/"), suffix)
	if id == "" || strings.Contains(id, "/") {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "红冲申请标识不合法")
		return
	}
	var in service.InvoiceRedFlushReviewInput
	if !decode(w, r, &in) {
		return
	}
	if err := a.service.ReviewInvoiceRedFlush(r.Context(), p, id, approve, in); err != nil {
		respondError(w, err)
		return
	}
	status := "REJECTED"
	if approve {
		status = "ISSUE_PENDING"
	}
	write(w, http.StatusOK, map[string]string{"status": status})
}

func buyerSummary(raw []byte) (string, string) {
	var buyer struct {
		Name  string `json:"name"`
		TaxNo string `json:"tax_no"`
	}
	_ = json.Unmarshal(raw, &buyer)
	return buyer.Name, buyer.TaxNo
}

func maskTaxNo(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 4 {
		return value
	}
	if len(value) <= 8 {
		return value[:2] + "****" + value[len(value)-2:]
	}
	return value[:6] + "****" + value[len(value)-2:]
}

func taxRateDisplay(value string, count int) string {
	if count == 0 || strings.TrimSpace(value) == "" {
		return "—"
	}
	if count > 1 {
		return "多税率"
	}
	rate, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return "—"
	}
	return fmt.Sprintf("%g%%", rate*100)
}
func (a *API) createDunningPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.manage")
	if !ok {
		return
	}
	var in service.DunningPolicyInput
	if !decode(w, r, &in) {
		return
	}
	recipient := strings.TrimSpace(strings.TrimPrefix(in.RecipientRule, "USER:"))
	if recipient == "" || recipient == in.RecipientRule {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "催收提醒人员无效")
		return
	}
	if a.directory == nil && !a.cfg.DevelopmentAuth {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_PERSONNEL_DIRECTORY_UNAVAILABLE", "催收人员目录尚未配置")
		return
	}
	if a.directory != nil {
		eligible, err := a.directory.ContainsCollector(r.Context(), recipient)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_PERSONNEL_DIRECTORY_UNAVAILABLE", "催收人员目录暂不可用")
			return
		}
		if !eligible {
			fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_RECIPIENT", "提醒人员必须是当前系统中有效的催收人员")
			return
		}
	}
	id, err := a.service.CreateDunningPolicy(r.Context(), p, in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusCreated, map[string]string{"id": id})
}

func (a *API) listDunningRecipients(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.user(w, r, "settlement.dunning.manage"); !ok {
		return
	}
	if a.directory == nil {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_PERSONNEL_DIRECTORY_UNAVAILABLE", "催收人员目录尚未配置")
		return
	}
	page, pageSize := 1, 50
	if value := strings.TrimSpace(r.URL.Query().Get("page")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100000 {
			fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "页码无效")
			return
		}
		page = parsed
	}
	if value := strings.TrimSpace(r.URL.Query().Get("page_size")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "分页大小无效")
			return
		}
		pageSize = parsed
	}
	items, total, err := a.directory.ListCollectors(r.Context(), r.URL.Query().Get("keyword"), page, pageSize)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "SETTLEMENT_PERSONNEL_DIRECTORY_UNAVAILABLE", "催收人员目录暂不可用")
		return
	}
	write(w, http.StatusOK, map[string]any{"items": items, "page": page, "page_size": pageSize, "total": total})
}
func (a *API) listDunningPolicies(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.manage")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT id,name,version,aging_from_days,aging_to_days,action_type,recipient_rule,channel,repeat_interval_days,priority,enabled FROM settlement_dunning_policy WHERE tenant_id=? ORDER BY aging_from_days`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, action, recipient, channel, priority string
		var version, from, to, repeat int
		var enabled bool
		if err := rows.Scan(&id, &name, &version, &from, &to, &action, &recipient, &channel, &repeat, &priority, &enabled); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "version": version, "aging_from_days": from, "aging_to_days": to, "action_type": action, "recipient_rule": recipient, "channel": channel, "repeat_interval_days": repeat, "priority": priority, "enabled": enabled})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) listDunningCases(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.manage")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT dc.id,r.receivable_no,cs.customer_name_snapshot,DATEDIFF(CURDATE(),r.due_date),r.open_amount,dc.current_escalation_level,dc.status,dc.next_action_at FROM settlement_dunning_case dc JOIN settlement_receivable r ON r.id=dc.receivable_id AND r.tenant_id=dc.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id WHERE dc.tenant_id=? ORDER BY dc.next_action_at LIMIT 100`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, customer, open, status string
		var days, level int
		var next any
		if err := rows.Scan(&id, &no, &customer, &days, &open, &level, &status, &next); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "receivable_no": no, "customer_name": customer, "aging_days": days, "open_amount": open, "level": level, "status": status, "next_action_at": next})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) listDunningActions(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.manage")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT da.id,r.receivable_no,da.channel,da.sent_at,da.created_at,da.recipient_user_id,da.priority,da.status,dc.current_escalation_level FROM settlement_dunning_action da JOIN settlement_dunning_case dc ON dc.id=da.dunning_case_id AND dc.tenant_id=da.tenant_id JOIN settlement_receivable r ON r.id=dc.receivable_id WHERE da.tenant_id=? ORDER BY COALESCE(da.sent_at,da.created_at) DESC LIMIT 100`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, receivableNo, channel, recipient, priority, status string
		var sentAt, createdAt any
		var level int
		if err := rows.Scan(&id, &receivableNo, &channel, &sentAt, &createdAt, &recipient, &priority, &status, &level); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "receivable_no": receivableNo, "channel": channel, "sent_at": sentAt, "created_at": createdAt, "recipient_user_id": recipient, "priority": priority, "status": status, "escalation_level": level})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) listNotifications(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.notification.read")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT id,notification_scope,priority,title,content,target_url,reference_type,reference_id,created_at,read_at FROM settlement_local_notification WHERE tenant_id=? AND recipient_user_id=? ORDER BY created_at DESC LIMIT 50`, p.TenantID, p.UserID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, scope, priority, title, content, target, refType, refID string
		var created any
		var read sql.NullTime
		if err := rows.Scan(&id, &scope, &priority, &title, &content, &target, &refType, &refID, &created, &read); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "scope": scope, "priority": priority, "title": title, "content": content, "target_url": target, "reference_type": refType, "reference_id": refID, "created_at": created, "read": read.Valid})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (a *API) unreadCount(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.notification.read")
	if !ok {
		return
	}
	var count int
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM settlement_local_notification WHERE tenant_id=? AND recipient_user_id=? AND read_at IS NULL`, p.TenantID, p.UserID).Scan(&count); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]int{"unread_count": count})
}
func (a *API) readNotification(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.notification.read")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/notifications/"), "/read")
	result, err := a.service.DB.ExecContext(r.Context(), `UPDATE settlement_local_notification SET read_at=COALESCE(read_at,UTC_TIMESTAMP(3)) WHERE id=? AND tenant_id=? AND recipient_user_id=?`, id, p.TenantID, p.UserID)
	if err != nil {
		respondError(w, err)
		return
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到通知")
		return
	}
	write(w, http.StatusOK, map[string]string{"status": "read"})
}
func (a *API) user(w http.ResponseWriter, r *http.Request, permission string) (service.Principal, bool) {
	p, err := a.principal(r)
	if err != nil {
		if errors.Is(err, platform.ErrUnauthenticated) {
			fail(w, http.StatusUnauthorized, "SETTLEMENT_UNAUTHENTICATED", "登录已失效，请重新登录")
		} else {
			fail(w, http.StatusServiceUnavailable, "SETTLEMENT_IDENTITY_UNAVAILABLE", "身份服务暂不可用")
		}
		return p, false
	}
	if !p.Allows(permission) {
		fail(w, http.StatusForbidden, "SETTLEMENT_FORBIDDEN", "当前用户没有执行此操作的权限")
		return p, false
	}
	return p, true
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "请求内容不合法")
		return false
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}
func respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalid):
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", err.Error())
	case errors.Is(err, service.ErrNotFound):
		fail(w, http.StatusNotFound, "SETTLEMENT_NOT_FOUND", "未找到记录")
	case errors.Is(err, service.ErrConflict):
		fail(w, http.StatusConflict, "SETTLEMENT_CONFLICT", "记录已变化、重复或余额不足")
	default:
		fail(w, http.StatusInternalServerError, "SETTLEMENT_INTERNAL_ERROR", "服务处理失败")
	}
}
func write(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}
