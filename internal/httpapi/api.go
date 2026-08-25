package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/platform"
	"github.com/j-s-te/settlement/internal/service"
)

type API struct {
	service   *service.Service
	cfg       config.Config
	logger    *slog.Logger
	auth      *platform.Authenticator
	machine   *platform.ServiceTokenVerifier
	directory platform.PersonnelDirectory
}

func New(db *sql.DB, cfg config.Config, logger *slog.Logger, auth *platform.Authenticator, machine *platform.ServiceTokenVerifier, directories ...platform.PersonnelDirectory) http.Handler {
	var directory platform.PersonnelDirectory
	if len(directories) > 0 {
		directory = directories[0]
	}
	return &API{service: &service.Service{DB: db}, cfg: cfg, logger: logger, auth: auth, machine: machine, directory: directory}
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
	case r.URL.Path == "/auth/me" || r.URL.Path == "/api/v1/auth/me":
		a.me(w, r)
		return
	case r.URL.Path == "/logged-out":
		write(w, http.StatusOK, map[string]string{"status": "logged_out"})
		return
	case r.URL.Path == "/internal/v1/settlement/events/contracts" && r.Method == http.MethodPost:
		a.contractEvent(w, r)
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
	case strings.HasPrefix(r.URL.Path, "/api/v1/reports/exports/") && strings.HasSuffix(r.URL.Path, "/download") && r.Method == http.MethodGet:
		a.downloadReportExport(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/reports/exports/") && r.Method == http.MethodGet:
		a.reportExport(w, r)
		return
	case r.URL.Path == "/api/v1/reports/invoiced-receivables-top10" && r.Method == http.MethodGet:
		a.invoicedReceivablesTop10(w, r)
		return
	case r.URL.Path == "/api/v1/reports/invoiced-receivables/export" && r.Method == http.MethodPost:
		a.createInvoicedReceivablesExport(w, r)
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
	case strings.HasPrefix(r.URL.Path, "/api/v1/tax-invoices/") && r.Method == http.MethodGet:
		a.taxInvoiceDetail(w, r)
		return
	case r.URL.Path == "/api/v1/invoice-requests" && r.Method == http.MethodPost:
		a.createInvoiceRequest(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/invoice-requests/") && strings.HasSuffix(r.URL.Path, "/approve") && r.Method == http.MethodPost:
		a.approveInvoiceRequest(w, r)
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
	return service.Principal{TenantID: "dev", UserID: "dev-finance", Permissions: map[string]bool{"settlement.admin": true}}, nil
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
	write(w, http.StatusOK, map[string]any{"tenant_id": p.TenantID, "user_id": p.UserID, "permissions": permissions})
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
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT r.id,r.receivable_no,cs.source_contract_no,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.open_amount,r.currency,r.collection_status,r.invoice_status FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id WHERE r.tenant_id=? AND r.recognition_status='CONFIRMED' AND r.open_amount>0 AND r.invoice_status<>'NOT_INVOICED' ORDER BY r.open_amount DESC,r.due_date ASC,r.id ASC LIMIT 10`, p.TenantID)
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
	p, ok := a.user(w, r, "settlement.report.export")
	if !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		fail(w, http.StatusBadRequest, "SETTLEMENT_INVALID_REQUEST", "需要有效的 Idempotency-Key")
		return
	}
	var id string
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT id FROM settlement_report_export_job WHERE tenant_id=? AND requested_by=? AND idempotency_key=?`, p.TenantID, p.UserID, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = newRequestID()
		_, err = a.service.DB.ExecContext(r.Context(), `INSERT INTO settlement_report_export_job(id,tenant_id,requested_by,report_type,idempotency_key,status,created_at,updated_at) VALUES(?,?,?,?,?,'PENDING',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, p.TenantID, p.UserID, "INVOICED_RECEIVABLES_CSV", key)
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
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT rp.id,cs.source_contract_no,cs.customer_name_snapshot,rp.installment_no,DATE_FORMAT(rp.due_date,'%Y-%m-%d'),rp.planned_amount,rp.confirmation_status,rp.version FROM settlement_receivable_plan rp JOIN settlement_contract_snapshot cs ON cs.id=rp.contract_snapshot_id WHERE rp.tenant_id=? ORDER BY rp.due_date ASC LIMIT 100`, p.TenantID)
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
	write(w, http.StatusOK, items)
}
func (a *API) confirmPlan(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.receivable.confirm")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/receivable-plans/"), "/confirm")
	var body struct {
		Version int `json:"version"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := a.service.ConfirmPlan(r.Context(), p, id, body.Version); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]string{"status": "confirmed"})
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
	query := `SELECT r.id,r.receivable_no,r.contract_snapshot_id,cs.source_contract_no,cs.customer_id,cs.customer_name_snapshot,DATE_FORMAT(r.due_date,'%Y-%m-%d'),r.original_amount,r.open_amount,r.currency,r.collection_status,r.invoice_status,r.version, r.original_amount-r.invoiced_amount-COALESCE((SELECT SUM(a.reserved_amount-a.invoiced_amount-a.released_amount) FROM settlement_invoice_request_allocation a WHERE a.tenant_id=r.tenant_id AND a.receivable_id=r.id AND a.status='RESERVED'),0) FROM settlement_receivable r JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id WHERE ` + strings.Join(where, " AND ") + ` ORDER BY r.due_date ASC,r.id ASC LIMIT ? OFFSET ?`
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
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT a.id,a.allocation_no,rc.receipt_no,rv.receivable_no,a.allocated_amount,a.currency,a.status,a.confirmed_by,a.confirmed_at FROM settlement_receipt_allocation a JOIN settlement_receipt rc ON rc.id=a.receipt_id JOIN settlement_receivable rv ON rv.id=a.receivable_id WHERE a.tenant_id=? ORDER BY a.confirmed_at DESC LIMIT 100`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, receipt, receivable, amount, currency, status, by string
		var at any
		if err := rows.Scan(&id, &no, &receipt, &receivable, &amount, &currency, &status, &by, &at); err != nil {
			respondError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "allocation_no": no, "receipt_no": receipt, "receivable_no": receivable, "amount": amount, "currency": currency, "status": status, "confirmed_by": by, "confirmed_at": at})
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
	p, ok := a.user(w, r, "settlement.invoice.request")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT ir.id,ir.request_no,cs.source_contract_no,ir.buyer_profile_snapshot,ir.invoice_type,ir.amount_excl_tax,ir.tax_amount,ir.amount_incl_tax,ir.status,ir.submitted_by,ir.approved_by,ir.version,ir.created_at,(SELECT COUNT(DISTINCT ii.tax_rate) FROM settlement_invoice_request_item ii WHERE ii.tenant_id=ir.tenant_id AND ii.invoice_request_id=ir.id),(SELECT MIN(ii.tax_rate) FROM settlement_invoice_request_item ii WHERE ii.tenant_id=ir.tenant_id AND ii.invoice_request_id=ir.id) FROM settlement_invoice_request ir JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id WHERE ir.tenant_id=? ORDER BY ir.created_at DESC LIMIT 100`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, no, contract, kind, excl, tax, incl, status, submitted, approved string
		var buyer []byte
		var taxRate sql.NullString
		var taxRateCount int
		var version int
		var created any
		if err := rows.Scan(&id, &no, &contract, &buyer, &kind, &excl, &tax, &incl, &status, &submitted, &approved, &version, &created, &taxRateCount, &taxRate); err != nil {
			respondError(w, err)
			return
		}
		buyerName, buyerTaxNo := buyerSummary(buyer)
		items = append(items, map[string]any{"id": id, "request_no": no, "contract_no": contract, "buyer_name": buyerName, "buyer_tax_no_masked": maskTaxNo(buyerTaxNo), "invoice_type": kind, "amount_excl_tax": excl, "tax_amount": tax, "amount_incl_tax": incl, "tax_rate_display": taxRateDisplay(taxRate.String, taxRateCount), "status": status, "submitted_by": submitted, "approved_by": approved, "version": version, "created_at": created})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (a *API) listTaxInvoices(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.request")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT ti.id,ti.invoice_request_id,ti.invoice_code,ti.invoice_no,cs.source_contract_no,ti.buyer_profile_snapshot,ti.invoice_type,ti.amount_incl_tax,DATE_FORMAT(ti.issue_date,'%Y-%m-%d'),ti.status,ti.issued_by_channel FROM settlement_tax_invoice ti JOIN settlement_invoice_request ir ON ir.id=ti.invoice_request_id AND ir.tenant_id=ti.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id WHERE ti.tenant_id=? ORDER BY ti.issue_date DESC,ti.created_at DESC LIMIT 100`, p.TenantID)
	if err != nil {
		respondError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, requestID, code, number, contract, kind, amount, issueDate, status, channel string
		var buyer []byte
		if err := rows.Scan(&id, &requestID, &code, &number, &contract, &buyer, &kind, &amount, &issueDate, &status, &channel); err != nil {
			respondError(w, err)
			return
		}
		buyerName, buyerTaxNo := buyerSummary(buyer)
		items = append(items, map[string]any{"id": id, "invoice_request_id": requestID, "invoice_code": code, "invoice_no": number, "contract_no": contract, "buyer_name": buyerName, "buyer_tax_no_masked": maskTaxNo(buyerTaxNo), "invoice_type": kind, "amount_incl_tax": amount, "issue_date": issueDate, "status": status, "issued_by_channel": channel})
	}
	if err := rows.Err(); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (a *API) taxInvoiceDetail(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.invoice.request")
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
	err := a.service.DB.QueryRowContext(r.Context(), `SELECT ti.id,ti.invoice_request_id,ir.request_no,ti.invoice_code,ti.invoice_no,cs.source_contract_no,ti.buyer_profile_snapshot,ti.seller_profile_snapshot,ti.invoice_type,ti.amount_excl_tax,ti.tax_amount,ti.amount_incl_tax,DATE_FORMAT(ti.issue_date,'%Y-%m-%d'),ti.status,ti.issued_by_channel FROM settlement_tax_invoice ti JOIN settlement_invoice_request ir ON ir.id=ti.invoice_request_id AND ir.tenant_id=ti.tenant_id JOIN settlement_contract_snapshot cs ON cs.id=ir.contract_snapshot_id WHERE ti.id=? AND ti.tenant_id=?`, id, p.TenantID).Scan(&invoiceID, &requestID, &requestNo, &code, &number, &contract, &buyer, &seller, &kind, &excl, &tax, &incl, &issueDate, &status, &channel)
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
	var documentCount int
	if err := a.service.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM settlement_invoice_document WHERE tenant_id=? AND tax_invoice_id=?`, p.TenantID, invoiceID).Scan(&documentCount); err != nil {
		respondError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"id": invoiceID, "invoice_request_id": requestID, "request_no": requestNo, "invoice_code": code, "invoice_no": number, "contract_no": contract, "buyer_name": buyerName, "buyer_tax_no_masked": maskTaxNo(buyerTaxNo), "seller_name": sellerName, "invoice_type": kind, "amount_excl_tax": excl, "tax_amount": tax, "amount_incl_tax": incl, "issue_date": issueDate, "status": status, "issued_by_channel": channel, "document_count": documentCount, "items": items})
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
	write(w, http.StatusOK, items)
}
func (a *API) listDunningCases(w http.ResponseWriter, r *http.Request) {
	p, ok := a.user(w, r, "settlement.dunning.manage")
	if !ok {
		return
	}
	rows, err := a.service.DB.QueryContext(r.Context(), `SELECT dc.id,r.receivable_no,cs.customer_name_snapshot,DATEDIFF(CURDATE(),r.due_date),r.open_amount,dc.current_escalation_level,dc.status,dc.next_action_at FROM settlement_dunning_case dc JOIN settlement_receivable r ON r.id=dc.receivable_id JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id WHERE dc.tenant_id=? ORDER BY dc.next_action_at LIMIT 100`, p.TenantID)
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
