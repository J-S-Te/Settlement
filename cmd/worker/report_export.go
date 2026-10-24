package main

import (
	"database/sql"
	"errors"
	"os"
	"strings"

	"github.com/j-s-te/settlement/internal/reportexport"
)

func newReportExporter(db *sql.DB, workerID string) (*reportexport.Worker, error) {
	// 工作器和 API 共享配置器的租户绑定。单个网关
	// 机器凭据绝不能发布其他租户的报告内容。
	tenantID := strings.TrimSpace(os.Getenv("OIDC_TENANT_ID"))
	if tenantID == "" {
		return nil, errors.New("OIDC_TENANT_ID is required for report export worker")
	}
	return &reportexport.Worker{DB: db, WorkerID: workerID, TenantID: tenantID, BatchSize: 5, GatewayMode: reportexport.NormalizeGatewayMode(os.Getenv("SETTLEMENT_FILE_GATEWAY_MODE")), GatewayApplicationID: strings.TrimSpace(os.Getenv("SETTLEMENT_FILE_GATEWAY_APPLICATION_ID"))}, nil
}
