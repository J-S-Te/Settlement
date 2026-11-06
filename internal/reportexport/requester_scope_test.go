package reportexport

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/service"
)

// unrestrictedRequesterScope 返回测试用的全范围申请人快照：结算财务经理在
// TENANT 范围持有 settlement.report.export，与创建任务时写入仓库的快照同构。
// 参数：t 为测试上下文，userID 为申请人。
// 返回值：requester_scope JSON 列值；编码失败即失败。
func unrestrictedRequesterScope(t *testing.T, userID string) []byte {
	t.Helper()
	scope, err := service.NewRequesterScope(service.Principal{
		UserID:         userID,
		Roles:          []string{"settlement_finance_manager"},
		Permissions:    map[string]bool{reportExportPermission: true},
		DataScopes:     []service.DataScope{{RoleCode: "settlement_finance_manager", ScopeType: "TENANT"}},
		CatalogVersion: "4",
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

// restrictedRequesterScope 返回持有导出权限但数据范围仅限单份合同的申请人快照。
// 参数：t 为测试上下文，userID 为申请人。
// 返回值：requester_scope JSON 列值；编码失败即失败。
func restrictedRequesterScope(t *testing.T, userID string) []byte {
	t.Helper()
	scope, err := service.NewRequesterScope(service.Principal{
		UserID:         userID,
		Roles:          []string{"settlement_accountant"},
		Permissions:    map[string]bool{reportExportPermission: true},
		DataScopes:     []service.DataScope{{RoleCode: "settlement_accountant", ScopeType: "CONTRACT", ScopeID: "contract-1"}},
		CatalogVersion: "4",
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

// failedMessages 返回记录 FAILED 更新绑定参数的断言辅助：返回 error_message 与任务ID。
// 参数：t 为测试上下文，d 为脚本驱动。
// 返回值：error_message 与任务ID 列表；FAILED 更新必须使用租约栅栏条件。
func failedMessages(t *testing.T, d *exportDB) ([]string, []string) {
	t.Helper()
	var messages, jobs []string
	for i, q := range d.execs {
		if !strings.Contains(q, "SET status='FAILED'") {
			continue
		}
		if !strings.Contains(q, "status='PROCESSING'") || !strings.Contains(q, "lease_generation=?") {
			t.Fatalf("FAILED update lost lease fencing: %s", q)
		}
		messages = append(messages, d.args[i][0].Value.(string))
		jobs = append(jobs, d.args[i][1].Value.(string))
	}
	return messages, jobs
}

// TestGenerateFailsClosedWithoutUnrestrictedRequesterScope 验证申请人范围快照缺失、
// 编码非法、申请人与任务归属不一致或仅有限范围时，任务被置为 FAILED 且不生成文件。
// 参数：t 为测试上下文。
// 返回值：无；生成文件、错误码不可识别或原因不可读时失败。
func TestGenerateFailsClosedWithoutUnrestrictedRequesterScope(t *testing.T) {
	validScope := unrestrictedRequesterScope(t, "user")
	cases := []struct {
		name  string
		scope []byte
		owner string
	}{
		{"legacy-null-snapshot", nil, "user"},
		{"corrupted-json", []byte(`{"user_id":`), "user"},
		{"empty-json-object", []byte(`{}`), "user"},
		{"requester-mismatch", validScope, "another-user"},
		{"restricted-contract-scope", restrictedRequesterScope(t, "user"), "user"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			d := &exportDB{}
			db := sql.OpenDB(d)
			defer db.Close()
			w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker"}
			err := w.generate(context.Background(), job{id: "job-1", tenantID: "tenant", reportType: InvoicedReceivablesCSV, generation: 7, requestedBy: test.owner, requesterScope: test.scope})
			if !errors.Is(err, errRequesterScope) {
				t.Fatalf("want sentinel requester scope error, got %v", err)
			}
			messages, jobs := failedMessages(t, d)
			if len(messages) != 1 || jobs[0] != "job-1" || messages[0] != requesterScopeFailureMessage {
				t.Fatalf("failed job not recorded with readable reason: messages=%v jobs=%v", messages, jobs)
			}
			for _, q := range d.execs {
				if strings.Contains(q, "SET status='READY'") || strings.Contains(q, "settlement_report_export_effect") {
					t.Fatalf("rejected requester scope still generated or published: %s", q)
				}
			}
			for _, q := range d.queries {
				if strings.Contains(q, "settlement_receivable") {
					t.Fatalf("rejected requester scope still queried tenant-wide receivables: %s", q)
				}
			}
		})
	}
}

// TestGeneratePublishesWithUnrestrictedRequesterScope 验证合法全范围快照仍正常生成。
// 参数：t 为测试上下文。
// 返回值：无；未发布 READY 或返回错误时失败。
func TestGeneratePublishesWithUnrestrictedRequesterScope(t *testing.T) {
	d := &exportDB{}
	db := sql.OpenDB(d)
	defer db.Close()
	w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker"}
	if err := w.generate(context.Background(), job{id: "job-1", tenantID: "tenant", reportType: InvoicedReceivablesCSV, generation: 7, requestedBy: "user", requesterScope: unrestrictedRequesterScope(t, "user")}); err != nil {
		t.Fatal(err)
	}
	ready, failed := false, false
	for _, q := range d.execs {
		if strings.Contains(q, "SET status='READY'") {
			ready = true
		}
		if strings.Contains(q, "SET status='FAILED'") {
			failed = true
		}
	}
	if !ready || failed {
		t.Fatalf("unrestricted requester did not publish: ready=%v failed=%v", ready, failed)
	}
}

// TestRunOnceSkipsBadScopeJobAndKeepsBatchMoving 验证一条历史无快照任务被置为 FAILED 后，
// 同批后续合法任务仍正常生成，坏任务不会卡住整个导出队列。
// 参数：t 为测试上下文。
// 返回值：无；批处理中断、合法任务未发布或失败原因不可读时失败。
func TestRunOnceSkipsBadScopeJobAndKeepsBatchMoving(t *testing.T) {
	claims := 0
	d := &exportDB{query: func(q string, _ []driver.NamedValue) (driver.Rows, error) {
		if strings.Contains(q, "SELECT e.job_id") {
			return &exportRows{columns: 3}, nil
		}
		if strings.Contains(q, "SELECT id,tenant_id,report_type") {
			claims++
			switch claims {
			case 1:
				// 历史任务：requester_scope 为 NULL，必须失败关闭。
				return &exportRows{columns: 6, values: [][]driver.Value{{"legacy-job", "tenant", InvoicedReceivablesCSV, int64(0), "user", nil}}}, nil
			case 2:
				return &exportRows{columns: 6, values: [][]driver.Value{{"scoped-job", "tenant", InvoicedReceivablesCSV, int64(0), "user", unrestrictedRequesterScope(t, "user")}}}, nil
			}
			return &exportRows{columns: 6}, nil
		}
		return &exportRows{columns: 9}, nil
	}}
	db := sql.OpenDB(d)
	defer db.Close()
	w := &Worker{DB: db, TenantID: "tenant", WorkerID: "worker", GatewayMode: "legacy"}
	err := w.RunOnce(context.Background())
	if !errors.Is(err, errRequesterScope) {
		t.Fatalf("bad scope job did not surface sentinel error: %v", err)
	}
	messages, jobs := failedMessages(t, d)
	if len(jobs) != 1 || jobs[0] != "legacy-job" || messages[0] != requesterScopeFailureMessage {
		t.Fatalf("legacy job not failed with readable reason: jobs=%v messages=%v", jobs, messages)
	}
	published := ""
	for i, q := range d.execs {
		if strings.Contains(q, "SET status='READY'") {
			published = d.args[i][7].Value.(string)
		}
	}
	if published != "scoped-job" {
		t.Fatalf("unrestricted job after bad scope job was not published: %q", published)
	}
}
