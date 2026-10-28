package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roleLandingSeed 创建超过旧50/100/200上限且账务相互一致的合成历史记录。
// 参数：t 为测试上下文，db 为独占迁移库，data 为浏览器业务标识映射。
// 返回值：无；任何写入错误停止测试，不接触现行业务库。
func roleLandingSeed(t *testing.T, db *sql.DB, data map[string]string) {
	t.Helper()
	data["landing"] = "1"
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for index := 0; index < 220; index++ {
		ids := map[string]string{"snapshot": roleSQLID(t), "receivable": roleSQLID(t), "receipt": roleSQLID(t), "allocation": roleSQLID(t), "difference": roleSQLID(t), "voucher": roleSQLID(t)}
		contract, project, stamp, date := fmt.Sprintf("qa-history-contract-%03d", index), fmt.Sprintf("qa-history-project-%03d", index), "2020-01-02 10:00:00", "2020-01-02"
		state, resolution, voucherState := "HELD", "", "NOT_CONFIGURED"
		if index == 219 {
			stamp, date, state, resolution, voucherState = "2019-01-02 10:00:00", "2019-01-02", "REVIEW_PENDING", "CARRY_FORWARD", "FAILED"
			for key, value := range ids {
				data["old_"+key+"_id"] = value
			}
			data["old_project_id"], data["old_contract_no"] = project, "QA-HISTORY-219"
		}
		payload, err := json.Marshal(map[string]string{"receipt_id": ids["receipt"], "receivable_id": ids["receivable"], "amount": "1.00", "currency": "CNY", "writeoff_id": ids["allocation"]})
		if err != nil {
			t.Fatal(err)
		}
		steps := []struct {
			query string
			args  []any
		}{
			{`INSERT INTO settlement_contract_snapshot(id,tenant_id,source_application,source_event_id,source_contract_id,source_contract_no,source_contract_version,customer_id,customer_name_snapshot,currency,contract_amount,financial_status,effective_at,created_at,updated_at) VALUES(?,'qa-role-ui','contract_management',?,?,?,1,'customer','隔离历史客户','CNY',100,'EFFECTIVE',?,?,?)`, []any{ids["snapshot"], roleSQLID(t), contract, fmt.Sprintf("QA-HISTORY-%03d", index), stamp, stamp, stamp}},
			{`INSERT INTO settlement_contract_responsibility VALUES('qa-role-ui',?,'sales-user',1,TRUE,?)`, []any{contract, stamp}},
			{`INSERT INTO settlement_project_reference VALUES('qa-role-ui',?,?,'customer',?,1,TRUE,?)`, []any{project, contract, project, stamp}},
			{`INSERT INTO settlement_project_assignment VALUES('qa-role-ui',?,'assistant-user',1,TRUE,'',?,'manager-user')`, []any{project, stamp}},
			{`INSERT INTO settlement_receivable(id,tenant_id,receivable_no,contract_snapshot_id,due_date,original_amount,received_allocated_amount,open_amount,currency,recognition_status,collection_status,invoice_status,created_at,updated_at) VALUES(?,'qa-role-ui',?,?,'2099-12-01',10,1,9,'CNY','CONFIRMED','PARTIALLY_SETTLED','NOT_INVOICED',?,?)`, []any{ids["receivable"], fmt.Sprintf("QA-H-RV-%03d", index), ids["snapshot"], stamp, stamp}},
			{`INSERT INTO settlement_receipt(id,tenant_id,receipt_no,customer_id,customer_name_snapshot,amount,unallocated_amount,currency,receipt_date,payment_method,bank_transaction_reference,source_type,status,idempotency_key,created_at,updated_at) VALUES(?,'qa-role-ui',?,'customer','隔离历史客户',20,17,'CNY',?,'BANK_TRANSFER',?,'MANUAL','PARTIALLY_ALLOCATED',?,?,?)`, []any{ids["receipt"], fmt.Sprintf("QA-H-RC-%03d", index), date, fmt.Sprintf("QA-H-BANK-%03d", index), fmt.Sprintf("qa-h-receipt-%03d", index), stamp, stamp}},
			{`INSERT INTO settlement_receipt_allocation(id,tenant_id,allocation_no,receipt_id,receivable_id,allocated_amount,currency,status,match_mode,confirmed_by,confirmed_at,idempotency_key,created_at) VALUES(?,'qa-role-ui',?,?,?,1,'CNY','CONFIRMED','MANUAL','accountant-user',?,?,?)`, []any{ids["allocation"], fmt.Sprintf("QA-H-AL-%03d", index), ids["receipt"], ids["receivable"], stamp, fmt.Sprintf("qa-h-allocation-%03d", index), stamp}},
			{`INSERT INTO settlement_receipt_difference(id,tenant_id,receipt_id,kind,amount,reason,status,requested_by,resolved_by,resolution,created_at) VALUES(?,'qa-role-ui',?,'UNKNOWN',2,'隔离历史暂挂',?,'accountant-user','accountant-user',?,?)`, []any{ids["difference"], ids["receipt"], state, resolution, stamp}},
			{`INSERT INTO settlement_accounting_voucher(id,tenant_id,writeoff_id,payload,status,created_at) VALUES(?,'qa-role-ui',?,?,?,?)`, []any{ids["voucher"], ids["allocation"], payload, voucherState, stamp}},
		}
		for _, step := range steps {
			if _, err := tx.Exec(step.query, step.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	data["http_refund_id"] = roleSQLID(t)
	if _, err = tx.Exec(`INSERT INTO settlement_receipt_difference(id,tenant_id,receipt_id,kind,amount,reason,status,resolution,requested_by,resolved_by,approved_by,created_at) VALUES(?,'qa-role-ui',?,'UNKNOWN',1,'隔离HTTP退款','REFUND_PENDING','REFUND_PENDING','accountant-user','accountant-user','manager-user',UTC_TIMESTAMP(3))`, data["http_refund_id"], data["receipt_id"]); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE settlement_receipt SET unallocated_amount=unallocated_amount-1 WHERE id=?`, data["receipt_id"]); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// roleLandingDatabaseAssertions 独立检查大列表操作和多角色退款的最终持久化事实。
// 参数：t 为测试上下文，db 为独占库，data 为本轮种子标识。
// 返回值：无；状态、金额、分页对象或审计不符时失败。
func roleLandingDatabaseAssertions(t *testing.T, db *sql.DB, data map[string]string) {
	t.Helper()
	checks := []struct {
		query string
		args  []any
		want  string
	}{
		{`SELECT planned_amount FROM settlement_receivable_plan WHERE contract_snapshot_id=? AND generation_source='MANUAL'`, []any{data["old_snapshot_id"]}, "10.00"},
		{`SELECT note FROM settlement_project_assignment WHERE project_id=? AND user_id='assistant-user'`, []any{data["old_project_id"]}, "历史项目翻页后可维护"},
		{`SELECT status FROM settlement_receipt_difference WHERE id=?`, []any{data["old_difference_id"]}, "APPROVED"},
		{`SELECT unallocated_amount FROM settlement_receipt WHERE id=?`, []any{data["old_receipt_id"]}, "18.00"},
		{`SELECT CONCAT(COUNT(*),':',MAX(status)) FROM settlement_accounting_voucher WHERE writeoff_id=?`, []any{data["old_allocation_id"]}, "1:NOT_CONFIGURED"},
		{`SELECT CONCAT(status,':',refund_confirmed_by,':',amount) FROM settlement_receipt_difference WHERE tenant_id='qa-role-ui' AND refund_reference='QA-BROWSER-REFUND-001'`, nil, "REFUND_CONFIRMED:accountant-user:2.00"},
		{`SELECT COUNT(*) FROM settlement_audit_event WHERE tenant_id='qa-role-ui' AND action='SETTLEMENT_REFUND_CONFIRMED'`, nil, "2"},
	}
	for index, check := range checks {
		var value string
		if err := db.QueryRow(check.query, check.args...).Scan(&value); err != nil {
			t.Fatalf("landing SQL postcondition %d: %v", index, err)
		}
		if value != check.want {
			t.Fatalf("landing SQL postcondition %d got=%s want=%s", index, value, check.want)
		}
	}
	t.Log("landing independent SQL postconditions: 7 PASS")
}

// roleLandingRequest 通过内存中的真实隔离会话调用正式业务HTTP入口。
// 参数：t 为测试上下文，base 为服务，sessions 为会话，alias 为角色，method/path/body 为业务请求。
// 返回值：状态码及JSON响应；不输出Cookie或请求头。
func roleLandingRequest(t *testing.T, base string, sessions map[string]any, alias, method, path, body string) (int, map[string]any) {
	t.Helper()
	r, err := http.NewRequest(method, base+"/api/v1/"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", "http://127.0.0.1:18173")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", "1")
	r.AddCookie(&http.Cookie{Name: "settlement_session", Value: sessions[alias].(map[string]string)["cookie"]})
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(r)
	if err != nil {
		t.Fatal("isolated role HTTP request failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("HTTP %d invalid JSON", response.StatusCode)
	}
	return response.StatusCode, result
}

// roleLandingHTTPAssertions 核验旧记录分页、退款权限/输入/重复与真实SQL状态。
// 参数：t 为测试上下文，db 为独占库，base 为正式服务，sessions/data 为隔离身份与业务标识。
// 返回值：无；任何超界、状态或余额不变量错误均失败。
func roleLandingHTTPAssertions(t *testing.T, db *sql.DB, base string, sessions map[string]any, data map[string]string) {
	t.Run("history_finance_pagination", func(t *testing.T) {
		for _, kind := range []string{"receipt-differences", "accounting-vouchers"} {
			code, result := roleLandingRequest(t, base, sessions, "manager", "GET", kind+"?limit=50&offset=200", "")
			if code != 200 {
				t.Fatalf("%s HTTP=%d", kind, code)
			}
			items := result["data"].(map[string]any)["items"].([]any)
			if len(items) > 50 || len(items) == 0 {
				t.Errorf("%s page count=%d want 1..50", kind, len(items))
			}
			wanted := data["old_difference_id"]
			if kind == "accounting-vouchers" {
				wanted = data["old_voucher_id"]
			}
			found := false
			for _, item := range items {
				if item.(map[string]any)["id"] == wanted {
					found = true
				}
			}
			if !found {
				t.Errorf("%s oldest pending record beyond 200 missing", kind)
			}
		}
	})
	t.Run("refund_fact_confirmation", func(t *testing.T) {
		path := "receipt-differences/" + data["http_refund_id"] + "/confirm-refund"
		completed := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
		body, _ := json.Marshal(map[string]any{"version": 1, "reference": "QA-HTTP-REFUND-001", "completed_at": completed, "reason": "隔离测试线下凭证回录"})
		for _, item := range []struct {
			alias, body string
			want        int
		}{{"sales", string(body), 403}, {"accountant", `{"version":1,"reference":"","completed_at":"` + completed + `","reason":"测试"}`, 400}, {"accountant", string(body), 200}, {"accountant", string(body), 409}} {
			code, _ := roleLandingRequest(t, base, sessions, item.alias, "POST", path, item.body)
			if code != item.want {
				t.Errorf("refund %s HTTP=%d want=%d", item.alias, code, item.want)
			}
		}
		var state, reference, available string
		if err := db.QueryRow(`SELECT status,refund_reference FROM settlement_receipt_difference WHERE id=?`, data["http_refund_id"]).Scan(&state, &reference); err != nil {
			t.Fatal(err)
		}
		if state != "REFUND_CONFIRMED" || reference != "QA-HTTP-REFUND-001" {
			t.Errorf("refund fact=%s %s", state, reference)
		}
		if err := db.QueryRow(`SELECT unallocated_amount FROM settlement_receipt WHERE id=?`, data["receipt_id"]).Scan(&available); err != nil || available != "9.00" {
			t.Errorf("refund changed held balance: %s err=%v", available, err)
		}
	})
}
