package httpapi

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/j-s-te/settlement/internal/service"
)

// queryScoped 用途：把合同责任谓词插入分页之前，不对返回后数据补过滤。
// 参数：ctx为上下文，p为主体，permission为能力，query为固定列表SQL，args为租户及分页参数。
// 返回值：返回已限定责任范围的查询游标或真实错误。
func (a *API) queryScoped(ctx context.Context, p service.Principal, permission, query string, args ...any) (*sql.Rows, error) {
	clause, scopeArgs := p.ContractPredicate(permission, "cs")
	clause = "cs.migration_state='RELEASED' AND (" + clause + ")"
	index := strings.LastIndex(query, " ORDER BY")
	if index < 0 || len(args) < 2 {
		return nil, service.ErrInvalid
	}
	query = query[:index] + " AND " + clause + query[index:]
	params := append([]any{}, args[:len(args)-2]...)
	params = append(params, scopeArgs...)
	params = append(params, args[len(args)-2:]...)
	return a.service.DB.QueryContext(ctx, query, params...)
}

// recordAllowed 用途：统一直接ID的单据访问校验并隐藏不可见对象。
// 参数：w为响应，r为请求，p为主体，permission为能力，kind为资源类型，id为单据。
// 返回值：允许访问时真；失败已写入HTTP错误。
func (a *API) recordAllowed(w http.ResponseWriter, r *http.Request, p service.Principal, permission, kind, id string) bool {
	if err := a.service.RequireRecordAccess(r.Context(), p, permission, kind, id); err != nil {
		respondError(w, err)
		return false
	}
	return true
}
