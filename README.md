# 结算与开票管理子系统

本目录保存结算与开票子系统的重构设计。该子系统是财务业务域的权威系统，负责应收、开票、回款、核销、账龄与催收；基础平台只提供身份、授权、审计及重要通知管控。

## 当前实现

已提供独立 Go API、Worker 与统一前端模块的可运行闭环：合同财务事件 Inbox、可审计的应收计划调整确认、正式应收、回款登记与匹配建议、余额受控的回款核销/冲销、开票申请审批/驳回/手工登记、税控结果 Inbox 与失败/未知状态对账、红票结果回写和额度冲销、电子发票文件网关归档与下载、账龄催收、应收账龄 TOP10、异步 CSV 报表导出、本地通知，以及审计和高优先级通知 Outbox。运行入口为 `go run ./cmd/api`、`go run ./cmd/worker` 和 `go run ./cmd/migrate`。

发票开具渠道由 `SETTLEMENT_INVOICE_ISSUANCE_MODE` 明确选择：`manual`（默认）审批后只建立人工登记任务，不产生税控 Outbox；`tax_adapter` 审批后只产生税控任务，服务端拒绝人工登记。两种模式不能混用，避免税控恢复或补配凭据后重复开票。

本地联调可显式设置 `SETTLEMENT_DEVELOPMENT_AUTH=true`；生产环境默认失败关闭，使用独立 OIDC Authorization Code + PKCE 会话、在线授权上下文和带 scope 的服务令牌，不能把开发身份或静态集成令牌带入生产。平台审计/通知及税务适配器均通过带租约、退避、回执校验和死信状态的 Outbox Worker 异步连接。

## 运行

- 本地隔离运行：`docker compose -f compose.local.yaml up --build`，API 暴露在 `18085`，不会占用统一前端默认端口。
- 生产配置：复制 `.env.example` 到部署密钥系统，禁止提交真实 Client Secret、会话加密密钥和数据库密码。
- 上线顺序：先执行 `settlement-migrate`，成功后启动 `settlement-api`，最后启动 `settlement-worker`。API `/healthz` 检查进程，`/readyz` 检查数据库。
- 权限目录：`authz/permission-manifest.json` 由基础平台应用登记流程导入；结算系统只消费在线授权结果，不在前端推断权限。

合同联调的事件类型固定为 `contract.financial_effective.v1`。结算不会从合同管理的查询接口自动拉取 completed contract；要生成可开票应收，必须先由合同管理投递带付款条款的事件，再在结算侧完成应收计划确认。`SETTLEMENT_INTEGRATION_ENABLED=false` 时，事件接入和外部投递保持关闭。

报表导出采用“提交任务→后台 Worker 生成 CSV→查询任务状态→下载文件”的异步模式。前端不在请求线程内生成大文件，也不通过数据库直接插入模拟应收。

当前上线前风险：登出仅撤销结算本地会话，尚未调用 OIDC `end_session_endpoint`；登录登出路由需要按生产安全门禁限制方法和来源；真实税控、银行和 ERP Provider 仍需在其沙箱/预生产环境完成专项验收。

生产启用税控前需将开具模式设为 `tax_adapter`，同时配置命令 URL、状态查询 URL、Provider code、出站 Client Credentials，以及独立的结果回调机器身份。API 要求 `SETTLEMENT_TAX_RESULT_INGEST_ENABLED=true`；Worker 缺少命令/查询端点或凭据时拒绝启动。命令包含 event ID 与不可变票面快照；投递回执只进入 `ACCEPTED`，最终结果由回调或状态查询统一写入 Inbox。未知状态沿用同一 `external_request_id` 对账，不会自动新开第二张票。未启用税控时保持 `manual`；人工模式禁止批准红冲，避免生成无人消费的税控任务。

电子发票归档需将 `SETTLEMENT_FILE_GATEWAY_MODE` 设置为 `dual` 或 `required`，并配置文件网关 URL、Client Credentials、应用 ID，以及 upload/bind/download scopes。红冲接口先创建 `SUBMITTED` 申请，由不同财务人员复核；只有复核通过才投递税控 Outbox。在有效红票回执落库前，原发票不会提前改成已红冲。

## 设计文档

- [总体设计方案](设计方案.md)：边界、状态机、集成、权限、安全、分期实施。
- [业务流程图](流程图/结算与开票管理子系统业务流程图.md)：合同生效到收款核销、催收的可执行流程。
- [实体关系图](流程图/结算与开票管理子系统ER图.md)：实施级逻辑模型。
- [原始 UI 原型](结算与开票管理子系统UI.html)：保留作为视觉和信息架构参考；实施时拆入现有 Vue 模块。
- [前端模块说明](../frontend/src/modules/settlement/README.md)：页面分区、按页加载、会话登出和测试边界。

> 原 `业务流程图.png` 和 `结算与开票管理子系统ER图.html` 均为重构前参考，不再作为实施依据。以本目录中的 Mermaid 图和总体设计方案为准。

## HTTP 接口速查

| 方法 | 路径 | 主要权限/用途 |
| --- | --- | --- |
| POST | `/internal/v1/settlement/events/contracts` | 机器调用；接收 `contract.financial_effective.v1` |
| POST | `/internal/v1/settlement/events/tax-results` | 独立税控机器身份；接收蓝票/红票成功、明确失败和未知结果 |
| GET | `/api/v1/dashboard` | `settlement.report.read`；结算总览 |
| GET/POST | `/api/v1/receivable-plans`、`/api/v1/receivable-plans/{id}/confirm` | 查询/确认应收计划 |
| GET | `/api/v1/receivables`、`/api/v1/invoice-eligible-receivables` | 查询应收/可开票应收 |
| GET/POST | `/api/v1/invoice-requests`、`/api/v1/invoice-requests/{id}/approve`、`/api/v1/invoice-requests/{id}/reject` | 发票申请、审批与驳回 |
| POST | `/api/v1/invoice-requests/{id}/manual-issue` | 审批后的人工发票登记 |
| GET | `/api/v1/tax-invoices`、`/api/v1/tax-invoices/{id}` | 发票台账与详情 |
| POST/GET | `/api/v1/tax-invoices/{id}/documents`、`/api/v1/tax-invoices/{id}/documents/{document_id}/download` | 电子发票归档与下载 |
| POST | `/api/v1/tax-invoices/{id}/red-flush` | 创建幂等红冲申请并等待财务复核 |
| POST | `/api/v1/invoice-red-flush-requests/{id}/approve`、`/api/v1/invoice-red-flush-requests/{id}/reject` | 财务复核红冲申请；通过后才投递税控命令 |
| GET/POST | `/api/v1/receipts`、`/api/v1/receipt-allocations` | 回款登记和分配 |
| GET | `/api/v1/receipts/{id}/matches` | 查询同客户、同币种的可核销应收建议 |
| GET/POST | `/api/v1/dunning/*` | 账龄、催收案件和策略 |
| GET/POST | `/api/v1/reports/*` | 异步报表任务、状态和下载 |
| GET/POST | `/api/v1/notifications/*` | 本地结算通知 |

写操作使用当前子系统会话、CSRF 和 `Idempotency-Key`（具体例外以 Handler 为准）；所有租户、权限和数据范围由后端判定。接口字段发生变化时，应同步本表、前端 README 和对应测试。
