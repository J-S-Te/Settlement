package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/httpapi"
	"github.com/j-s-te/settlement/internal/service"
)

// TestRedTaxInvoiceDetailUsesIssuedSignedItems 需显式启用并使用数据库。
// 在红冲回调后请求真实详情端点，证明
// 返回红票自身带符号明细，而非正数申请明细；
// 同时申请明细仍可作为依据查看。
func TestRedTaxInvoiceDetailUsesIssuedSignedItems(t *testing.T) {
	if os.Getenv("SETTLEMENT_E2E_TEST") != "1" {
		t.Skip("set SETTLEMENT_E2E_TEST=1 to run the database-backed red invoice detail flow")
	}
	dsn := strings.TrimSpace(os.Getenv("SETTLEMENT_E2E_TEST_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("SETTLEMENT_MYSQL_DSN"))
	}
	if dsn == "" {
		t.Fatal("SETTLEMENT_E2E_TEST_DSN or SETTLEMENT_MYSQL_DSN is required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		t.Fatalf("database is not reachable: %v", err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	tenant := "e2e-red-detail-" + suffix
	principal := service.Principal{TenantID: tenant, UserID: "e2e-finance", Permissions: map[string]bool{"settlement.admin": true}}
	reviewer := service.Principal{TenantID: tenant, UserID: "e2e-reviewer", Permissions: map[string]bool{"settlement.admin": true}}
	svc := &service.Service{DB: db, InvoiceIssuanceMode: "tax_adapter", TaxProviderCode: "e2e-provider"}
	receivableID, snapshotID := createReceivable(t, ctx, svc, principal, "CNY", "red"+suffix)
	blueInvoiceID := issueTaxInvoice(t, ctx, db, svc, principal, snapshotID, receivableID, "100.00", "BLUE-"+suffix)

	redRequestID, err := svc.RequestInvoiceRedFlush(ctx, principal, "red-detail-request-"+suffix, blueInvoiceID, service.InvoiceRedFlushInput{ReasonCode: "INVOICE_ERROR", ReasonDetail: "E2E red invoice detail"})
	if err != nil {
		t.Fatalf("request red flush: %v", err)
	}
	if err = svc.ReviewInvoiceRedFlush(ctx, reviewer, redRequestID, true, service.InvoiceRedFlushReviewInput{Version: 1}); err != nil {
		t.Fatalf("approve red flush: %v", err)
	}
	var redAttemptID string
	if err = db.QueryRowContext(ctx, `SELECT id FROM settlement_invoice_red_flush_attempt WHERE tenant_id=? AND red_flush_request_id=?`, tenant, redRequestID).Scan(&redAttemptID); err != nil {
		t.Fatal(err)
	}
	redEvent := service.TaxCallbackEvent{
		EventID: "tax-red-detail-" + suffix, EventType: service.TaxInvoiceRedFlushedEvent, TenantID: tenant,
		ProviderCode: "e2e-provider", OccurredAt: time.Now().UTC(), ResultSequence: 1,
		RedFlushAttemptID: redAttemptID, RedFlushRequestID: redRequestID,
		ExternalRequestID: redAttemptID, ExternalOperationID: "RED-DETAIL-OP-" + suffix,
		Invoice: taxResult("100.00", "RED-DETAIL", "RED-DETAIL-NO", "RED-DETAIL-EXT-"+suffix),
	}
	if applied, callbackErr := svc.ApplyTaxCallback(ctx, tenant, redEvent); callbackErr != nil || applied != "APPLIED" {
		t.Fatalf("apply red flush: result=%q err=%v", applied, callbackErr)
	}
	var redInvoiceID string
	if err = db.QueryRowContext(ctx, `SELECT red_invoice_id FROM settlement_invoice_red_flush_attempt WHERE tenant_id=? AND id=?`, tenant, redAttemptID).Scan(&redInvoiceID); err != nil {
		t.Fatal(err)
	}
	if redInvoiceID == "" || redInvoiceID == blueInvoiceID {
		t.Fatalf("red invoice id=%q", redInvoiceID)
	}

	handler := httpapi.New(db, config.Config{DevelopmentAuth: true, PublicOrigin: "http://localhost:5173"}, slog.Default(), nil, nil, nil)
	type detailItem struct {
		AmountInclTax string `json:"amount_incl_tax"`
	}
	type detailBody struct {
		Data struct {
			AmountInclTax string       `json:"amount_incl_tax"`
			ItemsSource   string       `json:"items_source"`
			Items         []detailItem `json:"items"`
			RequestItems  []detailItem `json:"request_items"`
		} `json:"data"`
	}
	readDetail := func(invoiceID string) detailBody {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/tax-invoices/"+invoiceID, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("detail %s status=%d body=%s", invoiceID, response.Code, response.Body.String())
		}
		var body detailBody
		if err = json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	redDetail := readDetail(redInvoiceID)
	if redDetail.Data.AmountInclTax != "-100.00" || redDetail.Data.ItemsSource != "TAX_INVOICE" {
		t.Fatalf("red header=%q source=%q", redDetail.Data.AmountInclTax, redDetail.Data.ItemsSource)
	}
	if len(redDetail.Data.Items) != 1 || redDetail.Data.Items[0].AmountInclTax != "-100.00" {
		t.Fatalf("red issued items=%+v, want the stored negative invoice line", redDetail.Data.Items)
	}
	if len(redDetail.Data.RequestItems) != 1 || redDetail.Data.RequestItems[0].AmountInclTax != "100.00" {
		t.Fatalf("red request items=%+v, want the positive application basis", redDetail.Data.RequestItems)
	}

	blueDetail := readDetail(blueInvoiceID)
	if blueDetail.Data.AmountInclTax != "100.00" || blueDetail.Data.ItemsSource != "TAX_INVOICE" {
		t.Fatalf("blue header=%q source=%q", blueDetail.Data.AmountInclTax, blueDetail.Data.ItemsSource)
	}
	if len(blueDetail.Data.Items) != 1 || blueDetail.Data.Items[0].AmountInclTax != "100.00" {
		t.Fatalf("blue issued items=%+v", blueDetail.Data.Items)
	}
}
