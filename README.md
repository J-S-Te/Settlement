# 结算与开票管理子系统

本目录包含结算与开票子系统的 Go API、Worker、迁移及保留的设计资料。该子系统负责应收、开票、人工回款登记、核销、账龄与催收；基础平台提供身份、授权、审计及重要通知管控。

2026-10-22 已按页面、API、鉴权、业务事务、迁移和 Worker 装配核实当前操作；同日补齐下述 RP 退出链路与本地行为回归，未部署或执行真实 IdP/浏览器业务验收。角色、条件、步骤、成功判断与失败处理见[结算与开票操作手册](../sharedDocs/10_操作手册/结算与开票操作手册.md)，历史核验见[2026-10-13 台账](../.ai/modules/operation-docs-business-20261013.md)。静态记录见[2026-10-22 文档同步报告](../.ai/qa/docs-code-sync-20261022/agents/project-bi-settlement-report.json)，退出修复证据见[本轮任务记录](../.ai/qa/docs-gap-fixes-20261022/agents/project-settlement-triage.json)。

## 当前实现

已提供独立 Go API、Worker 与统一前端模块的可运行闭环：合同财务事件 Inbox、可审计的应收计划调整确认、正式应收、回款登记与匹配建议、余额受控的回款核销/冲销、开票申请审批/驳回/手工登记、税控结果 Inbox 与失败/未知状态对账、红票结果回写和额度冲销、电子发票文件网关归档与下载、账龄催收、应收账龄 TOP10、异步 CSV 报表导出、本地通知，以及审计和高优先级通知 Outbox。运行入口为 `go run ./cmd/api`、`go run ./cmd/worker` 和 `go run ./cmd/migrate`。

发票开具渠道由 `SETTLEMENT_INVOICE_ISSUANCE_MODE` 明确选择：`manual`（默认）审批后只建立人工登记任务，不产生税控 Outbox；`tax_adapter` 审批后只产生税控任务，服务端拒绝人工登记。两种模式不能混用，避免税控恢复或补配凭据后重复开票。

本地联调可显式设置 `SETTLEMENT_DEVELOPMENT_AUTH=true`；生产环境默认失败关闭，使用独立 OIDC Authorization Code + PKCE 会话、在线授权上下文和带 scope 的服务令牌，不能把开发身份或静态集成令牌带入生产。平台审计/通知及税务适配器均通过带租约、退避、回执校验和死信状态的 Outbox Worker 异步连接。

## 运行

- 本地隔离运行：`docker compose -f compose.local.yaml up --build`，API 暴露在 `18085`，不会占用统一前端默认端口。
- 生产配置：复制 `.env.example` 到部署密钥系统，禁止提交真实 Client Secret、会话加密密钥和数据库密码。
- 上线顺序：先执行 `settlement-migrate`，成功后启动 `settlement-api`，最后启动 `settlement-worker`。API `/healthz` 检查进程，`/readyz` 检查数据库。
- 权限目录：`authz/permission-manifest.json` 由基础平台应用登记流程导入；结算系统只消费在线授权结果，不在前端推断权限。

合同联调的事件类型固定为 `contract.financial_effective.v1`。结算不会从合同管理的查询接口自动拉取 completed contract；需受保护机器事件包含租户、客户、版本、稳定 event ID 和总额匹配的付款条款，再在结算侧确认计划生成正式应收。合同侧已实现人工付款计划、财务事件事务 Outbox、补建对账与投递消费者；只有合同满足生效资格、付款计划金额合计匹配合同金额，且合同出站 `SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED`、结算入站开关及独立机器身份/网络均可用，才会送达。无计划时先查付款计划、Outbox 和投递状态，不能以合同 active/签署 completed 推断自动生成应收。`SETTLEMENT_INTEGRATION_ENABLED=false` 时合同事件入口关闭；税控结果有独立开关和机器身份，其他外部投递应分别核对配置。

报表导出采用“提交任务→后台 Worker 生成 CSV→查询任务状态→下载文件”的异步模式。前端不在请求线程内生成大文件，也不通过数据库直接插入模拟应收。

导出 Worker 必须设置部署绑定的 `OIDC_TENANT_ID`（含 legacy 模式），缺失时拒绝启动；领取、执行和网关副作用补偿均限定该租户，不能用一套机器凭据处理其他租户。启用网关时应先应用迁移 `000014_report_export_effects.sql` 并提供上传请求持久退役和按资源幂等解除绑定接口；失败上传或绑定的补偿会持久重试，直接退役原请求 ID 后解除其文件绑定，不以文件可读性查询作前置，不重传已退役内容、不清理历史已完成导出或更改长期文件保留策略。

`/auth/logout` 先按会话哈希和配置租户撤销本地会话，再清 Cookie；存在可信加密 ID Token 且 Discovery 的 `end_session_endpoint` 为可信 issuer 同源 HTTP(S) 地址时，携带绑定当前 issuer/client/tenant/identity 的 `id_token_hint` 发起 RP 退出。HTTP 仍需既有显式允许条件；旧 ID Token 可用作 hint，不要求令牌仍未过期。退出后地址只取 `OIDC_POST_LOGOUT_REDIRECT_URI`，须与 `SETTLEMENT_PUBLIC_ORIGIN` 同源，且由 OP 继续精确核验登记值；不从请求 Host、查询参数或 `return_to` 推导地址或 hint。当前无安全相关退出回调副作用，不创建或透传 `state`。

缺失/无效 hint 或不可信端点时，仅完成本地退出并跳 `/settlement/logged-out`。`/auth/local-logout` 保持仅本地撤销并返回 204，不发起 OP 退出。两种退出在数据库撤销失败时均返回 503、保留 Cookie 供重试；发出 RP 重定向不证明 OP 或其他应用会话已撤销。统一前端退出按钮仍调用平台退出；登录登出路由仍需按生产安全门禁限制方法和来源，真实 IdP/跨应用退出及税控、银行、ERP Provider 均需专项验收。

生产启用税控前需将开具模式设为 `tax_adapter`，同时配置命令 URL、状态查询 URL、Provider code、出站 Client Credentials，以及独立的结果回调机器身份。API 要求 `SETTLEMENT_TAX_RESULT_INGEST_ENABLED=true`；Worker 缺少命令/查询端点或凭据时拒绝启动。命令包含 event ID 与不可变票面快照；投递回执只进入 `ACCEPTED`，最终结果由回调或状态查询统一写入 Inbox。未知状态沿用同一 `external_request_id` 对账，不会自动新开第二张票。明确失败且无票据/Provider冲突的申请，可由非申请人的审批者带版本和原因“失败复核”回待审核，之后另行审批。未启用税控时保持 `manual`；人工模式禁止批准红冲，避免生成无人消费的税控任务。

电子发票归档需将 `SETTLEMENT_FILE_GATEWAY_MODE` 设置为 `dual` 或 `required`，并配置文件网关 URL、Client Credentials、应用 ID，以及 upload/bind/download scopes。红冲接口先创建 `SUBMITTED` 申请，由不同财务人员复核；只有复核通过才投递税控 Outbox。在有效红票回执落库前，原发票不会提前改成已红冲。

电子发票文件使用现有 `settlement.invoice.document` purpose（`CONFIDENTIAL`、`LONG_TERM`）。启用新版归档前先应用 `000015_invoice_archive_effects.sql`，并确保网关已提供持久 RetireUpload 和精确资源 Release 墓碑接口。API 在外调前提交技术意图，再在有界 30 秒事务中锁定同租户发票和意图，执行上传/绑定；文档、归档审计和 `ARCHIVED` 意图同时提交。提交响应丢失不能证明回滚，Worker 只处理数据库中实际仍为 `PENDING` 的意图；成功文档不进入补偿候选。

归档 Worker 复用现有 `settlement-worker`、商业授权及 `OIDC_TENANT_ID`/网关机器身份，10 秒轮询；意图满 2 分钟后可恢复，事务执行中通过行锁跳过。恢复先持久退休原上传请求，再解除其精确文件/发票绑定，全部确认后才记 `RETIRED`；失败记录次数、延后 30 秒并报告错误。原始幂等键按租户哈希保留，跨发票或变更内容重用会冲突；相同发票/类型/checksum 并发只产生一份文档。已退休键须用新键重新发起，仍在归档或恢复的重复内容返回冲突。历史绑定不自动扫描或清理，补偿不删除长期字节。

意图快照保存应用和网关 URL；配置漂移时拒绝向新身份发送旧意图，并保留待恢复记录。关闭网关模式或缺少凭据时不会伪造恢复成功。变更部署租户、应用或端点前，应先在原身份下处理待恢复意图；本机制不自动迁移历史身份。

正常归档由 API 将实时商业授权检查接入 Service，在持久意图提交前、取得执行锁后上传前、绑定前和归档提交前复核；等待锁或外部服务期间到期时返回既有 `COMMERCIAL_LICENSE_DENIED`，已提交的 PENDING 仍供获准后的恢复 Worker 处理。API 缺少授权门禁时归档失败关闭。

已有相同 checksum 文档的重复归档，必须核对同租户文档的 `storage_object_id` 与其成功 ARCHIVED 日志来源，确认原应用、网关 URL、发票、类型和 checksum 一致后才保存新键别名，别名继承原文件身份。配置漂移或历史文档缺少可验证日志时返回409及“请管理员受控核对归档来源”提示，不猜当前网关归属、不自动补写历史来源或迁移旧数据；反复刷新或换键不能解决该情况。原文档查询和下载接口保持原有权限边界。

归档专项隔离测试：`SETTLEMENT_TEST_DSN` 仅接受 `settlement_e2e[_namespace]`，测试在该专用服务器新建随机独占 schema、应用真实迁移并仅清理自有 schema。执行 `go test ./internal/service -run 'TestMySQLInvoiceArchive|TestRealGatewayInvoiceArchiveRecovery' -count=1 -v`。真实网关验收另需显式 `SETTLEMENT_TEST_FILE_GATEWAY_URL`、`SETTLEMENT_TEST_FILE_GATEWAY_TOKEN`、`SETTLEMENT_TEST_FILE_GATEWAY_TENANT_ID`、`SETTLEMENT_TEST_FILE_GATEWAY_APPLICATION_ID`；可提供严格测试库命名的 `FILE_GATEWAY_TEST_DSN` 做只读持久状态断言，及本轮隔离的 `FILE_GATEWAY_TEST_STORAGE_ROOT` 通过根目录约束读取原字节。解除唯一绑定后必须拒绝下载（403），字节保留以根内物理文件核验，不能放宽鉴权来检查。未配置时明确跳过，不回退业务 DSN。测试令牌只经环境/内存传递，不写入文档或日志。

## 票面与结清补偿

发票详情优先读取实际 `settlement_tax_invoice_item`，申请依据独立返回 `request_items`；逐行符号及三项合计须与票头一致，否则返回 409 `SETTLEMENT_TAX_INVOICE_ITEM_MISMATCH`。红票缺少实际明细拒绝展示，旧蓝票缺明细时保留申请明细回退并标记来源。

应收从结清转为未结清时，冲销按应收期查找当前结清事件：未投递事件取消，已投递/在途事件生成引用实际结清 allocation 的撤销事件，同一结清事实不会重复补偿。CRM 实际消费及在途投递竞态仍需专门验收。

## 设计文档

下列需求/设计/原型保留原用途，不作为当前功能已上线的证据；操作以手册与实际代码为准。

- [总体设计方案](设计方案.md)：边界、状态机、集成、权限、安全、分期实施。
- [业务流程图](流程图/结算与开票管理子系统业务流程图.md)：合同生效到收款核销、催收的可执行流程。
- [实体关系图](流程图/结算与开票管理子系统ER图.md)：实施级逻辑模型。
- [原始 UI 原型](结算与开票管理子系统UI.html)：保留作为视觉和信息架构参考；实施时拆入现有 Vue 模块。
- [前端模块说明](../frontend/src/modules/settlement/README.md)：页面分区、按页加载、会话登出和测试边界。

> 原 `业务流程图.png` 和 `结算与开票管理子系统ER图.html` 均为重构前参考；Mermaid 图和总体设计方案亦应与实际实现区分。

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
| POST | `/api/v1/invoice-requests/{id}/recover-failure` | 明确税控失败的受控复核，仅恢复待审核 |
| GET | `/api/v1/tax-invoices`、`/api/v1/tax-invoices/{id}` | 发票台账与详情 |
| POST/GET | `/api/v1/tax-invoices/{id}/documents`、`/api/v1/tax-invoices/{id}/documents/{document_id}/download` | 电子发票归档与下载 |
| POST | `/api/v1/tax-invoices/{id}/red-flush` | 创建幂等红冲申请并等待财务复核 |
| POST | `/api/v1/invoice-red-flush-requests/{id}/approve`、`/api/v1/invoice-red-flush-requests/{id}/reject` | 财务复核红冲申请；通过后才投递税控命令 |
| GET/POST | `/api/v1/receipts`、`/api/v1/receipt-allocations` | 回款登记和分配 |
| GET | `/api/v1/receipts/{id}/matches` | 查询同客户、同币种的可核销应收建议 |
| GET/POST | `/api/v1/dunning/*` | 账龄、催收案件和策略 |
| GET/POST | `/api/v1/reports/*` | 异步报表任务、状态和下载 |
| GET/POST | `/api/v1/notifications/*` | 本地结算通知 |

写操作使用当前子系统会话、同源 CSRF 和 `Idempotency-Key`（具体例外以 Handler 为准）；当前账务仓储主要执行租户过滤，不应推断已实现个人/组织/项目行范围。回款 GET/POST 均检查 `settlement.receipt.record`；核销查询、匹配和确认检查 `settlement.allocation.confirm`，冲销另需 `settlement.allocation.reverse`。申请人不能审批本人开票/红冲申请，原核销确认人不能冲销该分配，管理员兼容权限不豁免这些限制。

单期应收确认可调整金额，但没有跨期重新校验合同总额；需人工核对合计。回款登记不校验真实银行到账，核销冲销不是银行退款或发票红冲。催收扫描按策略间隔生成后续动作并递增升级级别，零余额自动关闭并取消待办动作，余额恢复后可重新打开案件；冻结接收人只延后不误派。业务列表按 limit/offset 分页（客户端默认50、最多100，offset至100000），页面局部筛选/KPI仍不代表全库；CSV 任务上限 10000 行、有效 7 天，仅申请人可下载。导出租约按任务逐个领取、心跳续租并以 owner/generation/期限封住迟到发布；过期接管和网关上传/绑定补偿持久化到 000014 的副作用表，不按开始时间猜任务失效。报表下载读任务 BLOB，电子发票下载走文件网关。
