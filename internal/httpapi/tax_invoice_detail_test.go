package httpapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/config"
)

// 通过真实 HTTP 处理器验证详情端点。驱动仅
// 编排持久化边界，证明返回了哪些已存储
// 明细，以及票头符号如何被强制校验。

type taxDetailQueryStep struct {
	match string
	rows  [][]driver.Value
}

type taxDetailConnector struct {
	t       *testing.T
	queries []taxDetailQueryStep
	calls   map[string]int
}

type taxDetailConn struct{ connector *taxDetailConnector }

func (c *taxDetailConnector) Connect(context.Context) (driver.Conn, error) {
	return &taxDetailConn{c}, nil
}
func (c *taxDetailConnector) Driver() driver.Driver { return taxDetailDriver{} }

type taxDetailDriver struct{}

func (taxDetailDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

func (c *taxDetailConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *taxDetailConn) Close() error              { return nil }
func (c *taxDetailConn) Begin() (driver.Tx, error) { return nil, errors.New("no transaction expected") }

// QueryContext 用途：编排详情SQL和默认字段策略的测试边界。
// 参数：上下文为测试请求，query为SQL，绑定参数由详情fixture确定。
// 返回值：返回预设持久化结果；未知SQL时测试失败。
func (c *taxDetailConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query == "SELECT settings_json,version FROM settlement_settings WHERE tenant_id=?" {
		return &taxDetailRows{}, nil
	}
	for _, step := range c.connector.queries {
		if strings.Contains(query, step.match) {
			c.connector.calls[step.match]++
			return &taxDetailRows{rows: step.rows}, nil
		}
	}
	c.connector.t.Fatalf("unexpected query: %s", query)
	return nil, nil
}

type taxDetailRows struct {
	rows [][]driver.Value
	read int
}

func (r *taxDetailRows) Columns() []string {
	if len(r.rows) == 0 {
		return []string{"unused"}
	}
	columns := make([]string, len(r.rows[0]))
	for index := range columns {
		columns[index] = "column"
	}
	return columns
}
func (r *taxDetailRows) Close() error { return nil }
func (r *taxDetailRows) Next(dest []driver.Value) error {
	if r.read >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.read])
	r.read++
	return nil
}

func taxDetailHeaderRow(id, requestID, requestNo, code, number, kind, excl, tax, incl, status, channel string) []driver.Value {
	buyer, _ := json.Marshal(map[string]any{"name": "客户", "tax_no": "91330000"})
	seller, _ := json.Marshal(map[string]any{"name": "销方"})
	return []driver.Value{
		id, requestID, requestNo, code, number, "HT-1", buyer, seller,
		kind, excl, tax, incl, "2026-10-18", status, channel,
	}
}

func taxDetailItemRow(line int64, name, classification, excl, rate, tax, incl string) []driver.Value {
	return []driver.Value{line, name, classification, excl, rate, tax, incl}
}

func requestTaxInvoiceDetail(t *testing.T, invoiceID string, steps []taxDetailQueryStep) *httptest.ResponseRecorder {
	t.Helper()
	connector := &taxDetailConnector{t: t, queries: steps, calls: map[string]int{}}
	db := sql.OpenDB(connector)
	defer db.Close()
	handler := New(db, config.Config{DevelopmentAuth: true}, slog.Default(), nil, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/tax-invoices/"+invoiceID, nil))
	return response
}

func taxDetailQueries(header []driver.Value, invoiceItems, requestItems [][]driver.Value) []taxDetailQueryStep {
	return []taxDetailQueryStep{
		{match: "FROM settlement_tax_invoice ti JOIN settlement_invoice_request ir", rows: [][]driver.Value{header}},
		{match: "FROM settlement_tax_invoice_item", rows: invoiceItems},
		{match: "FROM settlement_invoice_request_item", rows: requestItems},
		{match: "FROM settlement_invoice_document", rows: nil},
		{match: "FROM settlement_invoice_red_flush_request", rows: nil},
	}
}

type taxDetailResponse struct {
	Data struct {
		AmountInclTax string `json:"amount_incl_tax"`
		ItemsSource   string `json:"items_source"`
		Items         []struct {
			LineNo        int    `json:"line_no"`
			ItemName      string `json:"item_name"`
			AmountExclTax string `json:"amount_excl_tax"`
			TaxAmount     string `json:"tax_amount"`
			AmountInclTax string `json:"amount_incl_tax"`
		} `json:"items"`
		RequestItems []struct {
			LineNo        int    `json:"line_no"`
			ItemName      string `json:"item_name"`
			AmountInclTax string `json:"amount_incl_tax"`
		} `json:"request_items"`
	} `json:"data"`
}

func decodeTaxDetail(t *testing.T, response *httptest.ResponseRecorder) taxDetailResponse {
	t.Helper()
	var body taxDetailResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode detail response %q: %v", response.Body.String(), err)
	}
	return body
}

func TestRedTaxInvoiceDetailReturnsSignedIssuedItems(t *testing.T) {
	header := taxDetailHeaderRow("INV-RED", "REQ-1", "IR-1", "RED", "R-1", "VAT_SPECIAL", "-100.00", "-6.00", "-106.00", "ISSUED", "TAX_ADAPTER")
	stored := [][]driver.Value{taxDetailItemRow(1, "红冲票面服务", "30101", "-100.00", "0.060000", "-6.00", "-106.00")}
	requested := [][]driver.Value{taxDetailItemRow(1, "申请依据服务", "30101", "100.00", "0.060000", "6.00", "106.00")}
	response := requestTaxInvoiceDetail(t, "INV-RED", taxDetailQueries(header, stored, requested))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeTaxDetail(t, response)
	if body.Data.AmountInclTax != "-106.00" || body.Data.ItemsSource != "TAX_INVOICE" {
		t.Fatalf("header=%q source=%q", body.Data.AmountInclTax, body.Data.ItemsSource)
	}
	if len(body.Data.Items) != 1 || body.Data.Items[0].ItemName != "红冲票面服务" || body.Data.Items[0].AmountInclTax != "-106.00" || body.Data.Items[0].AmountExclTax != "-100.00" || body.Data.Items[0].TaxAmount != "-6.00" {
		t.Fatalf("issued items=%+v, want negative tax-invoice lines", body.Data.Items)
	}
	if len(body.Data.RequestItems) != 1 || body.Data.RequestItems[0].ItemName != "申请依据服务" || body.Data.RequestItems[0].AmountInclTax != "106.00" {
		t.Fatalf("request items=%+v, want separate positive application basis", body.Data.RequestItems)
	}
}

func TestBlueTaxInvoiceDetailKeepsSignedIssuedItems(t *testing.T) {
	header := taxDetailHeaderRow("INV-BLUE", "REQ-2", "IR-2", "BLUE", "B-1", "VAT_SPECIAL", "100.00", "6.00", "106.00", "ISSUED", "TAX_ADAPTER")
	stored := [][]driver.Value{taxDetailItemRow(1, "服务", "30101", "100.00", "0.060000", "6.00", "106.00")}
	requested := [][]driver.Value{taxDetailItemRow(1, "服务", "30101", "100.00", "0.060000", "6.00", "106.00")}
	response := requestTaxInvoiceDetail(t, "INV-BLUE", taxDetailQueries(header, stored, requested))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeTaxDetail(t, response)
	if body.Data.ItemsSource != "TAX_INVOICE" || len(body.Data.Items) != 1 || body.Data.Items[0].AmountInclTax != "106.00" {
		t.Fatalf("blue detail changed: source=%q items=%+v", body.Data.ItemsSource, body.Data.Items)
	}
}

func TestLegacyManualBlueTaxInvoiceFallsBackToRequestItems(t *testing.T) {
	header := taxDetailHeaderRow("INV-MANUAL", "REQ-3", "IR-3", "MAN", "M-1", "VAT_SPECIAL", "100.00", "6.00", "106.00", "ISSUED", "MANUAL")
	requested := [][]driver.Value{taxDetailItemRow(1, "人工登记服务", "30101", "100.00", "0.060000", "6.00", "106.00")}
	response := requestTaxInvoiceDetail(t, "INV-MANUAL", taxDetailQueries(header, nil, requested))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeTaxDetail(t, response)
	if body.Data.ItemsSource != "REQUEST" || len(body.Data.Items) != 1 || body.Data.Items[0].ItemName != "人工登记服务" {
		t.Fatalf("legacy manual fallback changed: source=%q items=%+v", body.Data.ItemsSource, body.Data.Items)
	}
}

func TestTaxInvoiceDetailRejectsHeaderItemSignMismatch(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		header []driver.Value
		items  [][]driver.Value
	}{
		{"red header with positive lines", taxDetailHeaderRow("INV-RED", "REQ-1", "IR-1", "RED", "R-1", "VAT_SPECIAL", "-100.00", "-6.00", "-106.00", "ISSUED", "TAX_ADAPTER"), [][]driver.Value{taxDetailItemRow(1, "红冲服务", "30101", "100.00", "0.060000", "6.00", "106.00")}},
		{"blue header with negative lines", taxDetailHeaderRow("INV-BLUE", "REQ-2", "IR-2", "BLUE", "B-1", "VAT_SPECIAL", "100.00", "6.00", "106.00", "ISSUED", "TAX_ADAPTER"), [][]driver.Value{taxDetailItemRow(1, "服务", "30101", "-100.00", "0.060000", "-6.00", "-106.00")}},
		{"line totals differ from header", taxDetailHeaderRow("INV-BLUE", "REQ-2", "IR-2", "BLUE", "B-1", "VAT_SPECIAL", "100.00", "6.00", "106.00", "ISSUED", "TAX_ADAPTER"), [][]driver.Value{taxDetailItemRow(1, "服务", "30101", "99.00", "0.060000", "6.00", "105.00")}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requested := [][]driver.Value{taxDetailItemRow(1, "申请服务", "30101", "100.00", "0.060000", "6.00", "106.00")}
			response := requestTaxInvoiceDetail(t, "INV-1", taxDetailQueries(scenario.header, scenario.items, requested))
			if response.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var failure struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Code != "SETTLEMENT_TAX_INVOICE_ITEM_MISMATCH" {
				t.Fatalf("failure=%+v err=%v", failure, err)
			}
		})
	}
}

func TestRedTaxInvoiceWithoutIssuedItemsIsRejected(t *testing.T) {
	header := taxDetailHeaderRow("INV-RED", "REQ-1", "IR-1", "RED", "R-1", "VAT_SPECIAL", "-100.00", "-6.00", "-106.00", "ISSUED", "TAX_ADAPTER")
	requested := [][]driver.Value{taxDetailItemRow(1, "红冲服务", "30101", "100.00", "0.060000", "6.00", "106.00")}
	response := requestTaxInvoiceDetail(t, "INV-RED", taxDetailQueries(header, nil, requested))
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
