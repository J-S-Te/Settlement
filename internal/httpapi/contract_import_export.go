package httpapi

import (
	"github.com/j-s-te/settlement/internal/service"
	"net/http"
	"strings"
)

// serveImportExports 为暂存工作台生成、查询和下载带版本的独立对账导出。
// 参数：w为响应，r为已通过许可及CSRF入口的请求。
// 返回值：属于导出路由时返回真，失败状态始终可查询。
func (a *API) serveImportExports(w http.ResponseWriter, r *http.Request) bool {
	prefix := "/api/v1/settlement/contract-imports/"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.Contains(r.URL.Path, "/exports") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) < 2 || parts[1] != "exports" {
		return false
	}
	p, ok := a.user(w, r, "settlement.import.reconcile")
	if !ok {
		return true
	}
	var result service.ImportExport
	var err error
	if len(parts) == 2 && r.Method == http.MethodPost {
		result, err = a.service.CreateImportExport(r.Context(), p, parts[0])
	} else if (len(parts) == 3 || len(parts) == 4 && parts[3] == "download") && r.Method == http.MethodGet {
		result, err = a.service.GetImportExport(r.Context(), p, parts[0], parts[2])
	} else {
		fail(w, http.StatusMethodNotAllowed, "SETTLEMENT_METHOD_NOT_ALLOWED", "请求方法不支持")
		return true
	}
	if err != nil {
		respondImportError(w, err)
		return true
	}
	if len(parts) == 4 {
		if result.Status != "SUCCEEDED" || len(result.Content) == 0 {
			respondError(w, service.ErrConflict)
			return true
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="import-reconciliation.csv"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Content)
		return true
	}
	write(w, http.StatusOK, result)
	return true
}
