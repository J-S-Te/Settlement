package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/j-s-te/settlement/internal/projectreference"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

type licenseExpiryResolver struct {
	*projectReferenceSyncResolver
	after func()
}

// Resolve 在模拟权威查询完成后触发许可状态变化。
// 参数：ctx 为上下文；tenant 和 project 为可信任务范围。
// 返回值：原夹具的权威事实或错误，不改变其身份校验语义。
func (r *licenseExpiryResolver) Resolve(ctx context.Context, tenant, project string) (projectreference.Reference, error) {
	ref, err := r.projectReferenceSyncResolver.Resolve(ctx, tenant, project)
	if r.after != nil {
		r.after()
	}
	return ref, err
}

// TestProjectReferenceSyncCannotCommitAfterLicenseExpiry 验证外部读取和事务期间到期均不落业务事实。
// 参数：t 为隔离 SQL 边界测试上下文，不连接实际数据库。
// 返回值：无；到期未上报、仍开启写事务或仍提交水位时失败。
func TestProjectReferenceSyncCannotCommitAfterLicenseExpiry(t *testing.T) {
	for _, stage := range []string{"after-resolve", "before-success-commit", "before-conflict-commit"} {
		t.Run(stage, func(t *testing.T) {
			active := true
			denied := errors.New("test project sync license expired")
			ref := syncReference(1, true)
			steps := []scriptdb.Step{{Kind: "query", SQL: "FROM settlement_project_reference ORDER BY updated_at",
				Rows: [][]driver.Value{{"tenant-a", "project-1"}}}}
			resolver := &licenseExpiryResolver{projectReferenceSyncResolver: &projectReferenceSyncResolver{
				refs: map[string]projectreference.Reference{"tenant-a/project-1": ref},
			}}
			if stage == "after-resolve" {
				resolver.after = func() { active = false }
			} else {
				if stage == "before-conflict-commit" {
					ref.ContractID = "contract-9"
					resolver.refs["tenant-a/project-1"] = ref
				}
				steps = append(steps,
					scriptdb.Step{Kind: "begin"},
					scriptdb.Step{Kind: "exec", SQL: "INSERT IGNORE INTO settlement_project_reference"},
					scriptdb.Step{Kind: "query", SQL: "SELECT version,source_contract_id,customer_id,project_name,active FROM settlement_project_reference",
						Rows: [][]driver.Value{{int64(1), "contract-1", "42", "项目一", true}}},
					scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_project_reference SET updated_at", After: func() { active = false }},
					scriptdb.Step{Kind: "rollback"},
				)
			}
			worker := &ProjectReferenceSyncWorker{DB: scriptdb.Open(t, steps...), Resolver: resolver,
				CheckBusinessLicense: func(context.Context) error {
					if !active {
						return denied
					}
					return nil
				}}
			if err := worker.RunOnce(t.Context()); !errors.Is(err, denied) {
				t.Fatalf("expiry was not enforced at %s: %v", stage, err)
			}
		})
	}
}
