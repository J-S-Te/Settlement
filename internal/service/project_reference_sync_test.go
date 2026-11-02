package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/settlement/internal/projectreference"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

// projectReferenceSyncResolver 是项目权威服务的受控替身：按租户/项目返回固定
// 引用或注入失败，并记录调用顺序，用于验证刷新器不误改本地快照。
type projectReferenceSyncResolver struct {
	refs  map[string]projectreference.Reference
	errs  map[string]error
	calls []string
}

// Resolve 返回夹具中的权威引用或注入错误。
// 参数：ctx 为上下文，tenant/project 为待解析标识。
// 返回值：权威引用；未配置或注入错误时返回失败。
func (r *projectReferenceSyncResolver) Resolve(_ context.Context, tenant, project string) (projectreference.Reference, error) {
	key := tenant + "/" + project
	r.calls = append(r.calls, key)
	if err := r.errs[key]; err != nil {
		return projectreference.Reference{}, err
	}
	ref, ok := r.refs[key]
	if !ok {
		return projectreference.Reference{}, errors.New("authoritative project reference not found")
	}
	return ref, nil
}

// syncReference 构造权威项目引用夹具。
// 参数：version 为权威版本，active 为权威是否有效。
// 返回值：固定租户/项目/合同/客户的引用。
func syncReference(version uint64, active bool) projectreference.Reference {
	return projectreference.Reference{TenantID: "tenant-a", ProjectID: "project-1", ContractID: "contract-1", CustomerID: "42", Name: "项目一", Version: version, Active: active}
}

// TestProjectReferenceSyncAdvancesWatermarkWithoutChangingFacts 验证权威事实未变时
// 只推进刷新水位（updated_at），不改合同、客户或 active，批次可轮转。
// 参数：t 为测试上下文。
// 返回值：无；写入事实或未推进水位时失败。
func TestProjectReferenceSyncAdvancesWatermarkWithoutChangingFacts(t *testing.T) {
	var candidateArgs, watermarkArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_project_reference ORDER BY updated_at", Args: &candidateArgs, Rows: [][]driver.Value{{"tenant-a", "project-1"}}},
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT IGNORE INTO settlement_project_reference"},
		scriptdb.Step{Kind: "query", SQL: "SELECT version,source_contract_id,customer_id,project_name,active FROM settlement_project_reference", Rows: [][]driver.Value{{int64(1), "contract-1", "42", "项目一", true}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_project_reference SET updated_at", Args: &watermarkArgs},
		scriptdb.Step{Kind: "commit"},
	)
	resolver := &projectReferenceSyncResolver{refs: map[string]projectreference.Reference{"tenant-a/project-1": syncReference(1, true)}}
	worker := &ProjectReferenceSyncWorker{DB: db, Resolver: resolver, BatchSize: 50}
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(resolver.calls) != 1 || resolver.calls[0] != "tenant-a/project-1" {
		t.Fatalf("权威调用=%+v", resolver.calls)
	}
	if len(candidateArgs) != 1 || candidateArgs[0].Value != int64(50) {
		t.Fatalf("批量上限绑定=%+v", candidateArgs)
	}
	if len(watermarkArgs) != 2 || watermarkArgs[0].Value != "tenant-a" || watermarkArgs[1].Value != "project-1" {
		t.Fatalf("刷新水位绑定=%+v", watermarkArgs)
	}
}

// TestProjectReferenceSyncDeactivatesInactiveAuthority 验证权威项目失效时本地快照
// 收口为 active=false，正是范围谓词停止放行的依据。
// 参数：t 为测试上下文。
// 返回值：无；失效未落库时失败。
func TestProjectReferenceSyncDeactivatesInactiveAuthority(t *testing.T) {
	var factArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_project_reference ORDER BY updated_at", Rows: [][]driver.Value{{"tenant-a", "project-1"}}},
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT IGNORE INTO settlement_project_reference"},
		scriptdb.Step{Kind: "query", SQL: "SELECT version,source_contract_id,customer_id,project_name,active FROM settlement_project_reference", Rows: [][]driver.Value{{int64(2), "contract-1", "42", "项目一", true}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_project_reference SET source_contract_id=?,customer_id=?,project_name=?,version=?,active=?", Args: &factArgs},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_project_reference SET updated_at"},
		scriptdb.Step{Kind: "commit"},
	)
	resolver := &projectReferenceSyncResolver{refs: map[string]projectreference.Reference{"tenant-a/project-1": syncReference(2, false)}}
	worker := &ProjectReferenceSyncWorker{DB: db, Resolver: resolver, BatchSize: 50}
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(factArgs) < 5 || factArgs[4].Value != false {
		t.Fatalf("失效项目必须以 active=false 落库: %+v", factArgs)
	}
}

// TestProjectReferenceSyncKeepsSnapshotWhenAuthorityFails 验证权威服务失败时
// 不写库、不推刷新水位、不把项目误置 inactive，下一轮重试。
// 参数：t 为测试上下文。
// 返回值：无；失败时仍写库或吞掉错误时失败。
func TestProjectReferenceSyncKeepsSnapshotWhenAuthorityFails(t *testing.T) {
	db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_project_reference ORDER BY updated_at", Rows: [][]driver.Value{{"tenant-a", "project-1"}}})
	resolver := &projectReferenceSyncResolver{errs: map[string]error{"tenant-a/project-1": errors.New("project service unavailable")}}
	worker := &ProjectReferenceSyncWorker{DB: db, Resolver: resolver, BatchSize: 50}
	err := worker.RunOnce(t.Context())
	if err == nil || !strings.Contains(err.Error(), "project service unavailable") {
		t.Fatalf("依赖失败必须上报: %v", err)
	}
}

// TestProjectReferenceSyncKeepsStoredFactsOnConflict 验证同版本更换合同属于永久
// 冲突：保留已对账事实，仅推进刷新水位避免饿死后续项目，并上报冲突。
// 参数：t 为测试上下文。
// 返回值：无；覆盖已对账映射或未推进水位时失败。
func TestProjectReferenceSyncKeepsStoredFactsOnConflict(t *testing.T) {
	var watermarkArgs []driver.NamedValue
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "query", SQL: "FROM settlement_project_reference ORDER BY updated_at", Rows: [][]driver.Value{{"tenant-a", "project-1"}}},
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT IGNORE INTO settlement_project_reference"},
		scriptdb.Step{Kind: "query", SQL: "SELECT version,source_contract_id,customer_id,project_name,active FROM settlement_project_reference", Rows: [][]driver.Value{{int64(1), "contract-1", "42", "项目一", true}}},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_project_reference SET updated_at", Args: &watermarkArgs},
		scriptdb.Step{Kind: "commit"},
	)
	changed := syncReference(1, true)
	changed.ContractID = "contract-9"
	resolver := &projectReferenceSyncResolver{refs: map[string]projectreference.Reference{"tenant-a/project-1": changed}}
	worker := &ProjectReferenceSyncWorker{DB: db, Resolver: resolver, BatchSize: 50}
	err := worker.RunOnce(t.Context())
	if !errors.Is(err, ErrProjectReferenceMismatch) {
		t.Fatalf("同版本更换合同必须上报映射冲突: %v", err)
	}
	if len(watermarkArgs) != 2 {
		t.Fatalf("冲突行仍需推进刷新水位: %+v", watermarkArgs)
	}
}

// TestProjectReferenceSyncBatchSizeIsBounded 验证批量不可能超过上限：越界或非法值
// 与 dunning/taxreconcile 一致回落到默认 50，绝不把客户端/配置值直接放大。
// 参数：t 为测试上下文。
// 返回值：无；批量未被夹紧时失败。
func TestProjectReferenceSyncBatchSizeIsBounded(t *testing.T) {
	for _, tc := range []struct {
		batch int
		want  int64
	}{{500, 50}, {201, 50}, {0, 50}, {-3, 50}, {200, 200}, {17, 17}} {
		var args []driver.NamedValue
		db := scriptdb.Open(t, scriptdb.Step{Kind: "query", SQL: "FROM settlement_project_reference ORDER BY updated_at", Args: &args})
		worker := &ProjectReferenceSyncWorker{DB: db, Resolver: &projectReferenceSyncResolver{}, BatchSize: tc.batch}
		if err := worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(args) != 1 || args[0].Value != tc.want {
			t.Fatalf("batch=%d 绑定=%+v，期望 %d", tc.batch, args, tc.want)
		}
	}
}

// TestProjectReferenceSyncSkipsWhenPreviousRunInProgress 验证单飞：上一轮未结束时
// 本轮直接跳过且不产生任何数据库读写。
// 参数：t 为测试上下文。
// 返回值：无；并发生效或仍未跳过时失败。
func TestProjectReferenceSyncSkipsWhenPreviousRunInProgress(t *testing.T) {
	db := scriptdb.Open(t)
	resolver := &projectReferenceSyncResolver{}
	worker := &ProjectReferenceSyncWorker{DB: db, Resolver: resolver, BatchSize: 50}
	worker.running.Lock()
	defer worker.running.Unlock()
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatalf("单飞跳过不得返回错误: %v", err)
	}
	if len(resolver.calls) != 0 {
		t.Fatalf("单飞跳过仍调用权威服务: %+v", resolver.calls)
	}
}

// TestProjectReferenceSyncRequiresLicenseAndConfiguration 验证许可失败与配置缺失
// 都不触发外部调用或数据库读写。
// 参数：t 为测试上下文。
// 返回值：无；门禁顺序不符时失败。
func TestProjectReferenceSyncRequiresLicenseAndConfiguration(t *testing.T) {
	resolver := &projectReferenceSyncResolver{}
	licensed := &ProjectReferenceSyncWorker{DB: scriptdb.Open(t), Resolver: resolver, CheckBusinessLicense: func(context.Context) error { return errors.New("license expired") }}
	if err := licensed.RunOnce(t.Context()); err == nil || len(resolver.calls) != 0 {
		t.Fatalf("许可失败必须阻止刷新: err=%v calls=%+v", err, resolver.calls)
	}
	if err := (&ProjectReferenceSyncWorker{}).RunOnce(t.Context()); err == nil {
		t.Fatal("缺少数据库或权威客户端必须报配置错误")
	}
}
