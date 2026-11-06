package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// TestFinancialSubscriptionExplicitPauseKeepsBacklog 验证正式停订阅不读取、不领取也不伪成功历史行。
// 参数：t 为测试上下文。
// 返回值：无；任何SQL或投递发生时失败。
func TestFinancialSubscriptionExplicitPauseKeepsBacklog(t *testing.T) {
	s := &Service{}
	s.ConfigureFinancialSources(true, false)
	if err := s.RetryCreditSync(context.Background(), "tenant", "event"); !errors.Is(err, ErrCreditSyncNotSubscribed) {
		t.Fatal(err)
	}
	if ok, err := s.creditEligibleSnapshot(context.Background(), nil, "tenant", "snapshot", "7"); err != nil || ok {
		t.Fatalf("eligible=%v err=%v", ok, err)
	}
	worker := &CreditSyncWorker{SubscriptionDisabled: true}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestFinancialSubscriptionRequiresTrustedContractCustomer 验证数字ID不足以创建CRM信用事实。
// 参数：t 为测试上下文。
// 返回值：无；本地NULL身份、第三方来源、错误命名空间被接纳时失败。
func TestFinancialSubscriptionRequiresTrustedContractCustomer(t *testing.T) {
	for _, tc := range []struct {
		name, basis, namespace, source string
		contract                       driver.Value
		want                           bool
	}{
		{"trusted legacy", "CONTRACT", "legacy", "contract_management", "contract-1", true},
		{"trusted crm", "CONTRACT", "crm", "contract_management", "contract-1", true},
		{"local null", "LOCAL_FINANCIAL", "settlement_local", "settlement_local", nil, false},
		{"untrusted numeric", "CONTRACT", "legacy", "third_party", "7", false},
		{"wrong namespace", "CONTRACT", "settlement_local", "contract_management", "7", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT basis_type,customer_namespace,source_application,source_contract_id", Rows: [][]driver.Value{{tc.basis, tc.namespace, tc.source, tc.contract}}}, scriptdb.Step{Kind: "commit"})
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			s := &Service{DB: db}
			s.ConfigureFinancialSources(true, true)
			ok, err := s.creditEligibleSnapshot(context.Background(), tx, "tenant", "snapshot", "7")
			if err != nil || ok != tc.want {
				t.Fatalf("eligible=%v err=%v", ok, err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if ok, err := (&Service{}).creditEligibleSnapshot(context.Background(), nil, "tenant", "snapshot", "7"); err != nil || !ok {
		t.Fatalf("零值兼容丢失 %v %v", ok, err)
	}
}

// TestFinancialSubscriptionReversalDoesNotCreatePausedDelivery 验证真实冲销可取消未发旧任务但不新建停订阅投递。
// 参数：t 为测试上下文。
// 返回值：无；伪造DELIVERED或插入新补偿行时失败。
func TestFinancialSubscriptionReversalDoesNotCreatePausedDelivery(t *testing.T) {
	for _, state := range []string{"DELIVERED", "PROCESSING", "PENDING", "RETRY_WAIT"} {
		t.Run(state, func(t *testing.T) {
			steps := []scriptdb.Step{{Kind: "begin"}, {Kind: "query", SQL: "SELECT completed.event_id", Rows: [][]driver.Value{{"old", creditEventCompleted, state, "allocation", []byte(`{"customer_id":7}`)}}}}
			if state == "PENDING" || state == "RETRY_WAIT" {
				steps = append(steps, scriptdb.Step{Kind: "exec", SQL: "SET status='CANCELLED'"})
			}
			steps = append(steps, scriptdb.Step{Kind: "commit"})
			db := scriptdb.Open(t, steps...)
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			id, err := enqueueCreditReversal(context.Background(), tx, "tenant", "allocation", "receivable", false)
			if err != nil || id != "" {
				t.Fatalf("id=%s err=%v", id, err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestNativeReceiptUsesRegistryName 验证本地回款落库使用服务端客户名称并保持普通财务事实。
// 参数：t 为测试上下文。
// 返回值：无；客户端名称覆盖登记名称或外部信用队列被触及时失败。
func TestNativeReceiptUsesRegistryName(t *testing.T) {
	var args []driver.NamedValue
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT name FROM settlement_financial_customer", Rows: [][]driver.Value{{"服务端客户"}}},
		// 人工回款先按内容摘要认领幂等键，再写回款事实。
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_idempotency_record"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_receipt(", Args: &args}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}, scriptdb.Step{Kind: "commit"})
	p := nativeTestPrincipal("settlement_accountant")
	p.Permissions["settlement.receipt.record"] = true
	s := &Service{DB: db}
	s.ConfigureFinancialSources(true, false)
	id, err := s.CreateReceipt(context.Background(), p, "receipt-key", ReceiptInput{CustomerID: NativeCustomerPrefix + nativeTestCustomerID, CustomerName: "客户端伪名称", Amount: "100.00", Currency: "CNY", ReceiptDate: "2020-02-01", PaymentMethod: "TRANSFER", BankReference: "bank-7"})
	if err != nil || id == "" {
		t.Fatalf("id=%s err=%v", id, err)
	}
	if args[4].Value != "服务端客户" {
		t.Fatalf("args=%+v", args)
	}
}

// TestNativeAllocationRetainsLocalMoneyWithoutCRM 验证本地客户核销走原余额更新与审计，不产生外部信用任务。
// 参数：t 为测试上下文。
// 返回值：无；未真实更新余额或假造CRM身份时失败。
func TestNativeAllocationRetainsLocalMoneyWithoutCRM(t *testing.T) {
	customer := NativeCustomerPrefix + nativeTestCustomerID
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin"}, scriptdb.Step{Kind: "query", SQL: "SELECT contract_snapshot_id", Rows: [][]driver.Value{{"snapshot"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT migration_state", Rows: [][]driver.Value{{"NORMAL"}}}, scriptdb.Step{Kind: "query", SQL: "SELECT migration_state", Rows: [][]driver.Value{{"NORMAL"}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT COUNT(*) FROM settlement_import_fact", Rows: [][]driver.Value{{int64(0)}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT unallocated_amount", Rows: [][]driver.Value{{"100.00", "CNY", "2020-02-01", customer}}},
		scriptdb.Step{Kind: "query", SQL: "SELECT r.open_amount", Rows: [][]driver.Value{{"100.00", "100.00", "CNY", customer, "2020-02-01", "", int64(1)}}},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_receipt_allocation"}, scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receipt SET unallocated_amount"}, scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_receivable SET open_amount"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_audit_event"}, scriptdb.Step{Kind: "commit"})
	s := &Service{DB: db}
	s.ConfigureFinancialSources(true, true)
	id, err := s.ConfirmAllocation(context.Background(), nativeTestPrincipal("settlement_finance_manager"), "allocate-key", AllocationInput{ReceiptID: "receipt", ReceivableID: "receivable", Amount: "100.00", MatchMode: "MANUAL"})
	if err != nil || id == "" {
		t.Fatalf("id=%s err=%v", id, err)
	}
}
