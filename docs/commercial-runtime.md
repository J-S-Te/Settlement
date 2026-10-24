# Commercial runtime enforcement

## 当前登记协议与兼容边界（2026-10-22）

受控交付使用目录 V1 / 组件登记协议 V2：分别下发
`COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL=2` 和
`COMMERCIAL_LICENSE_CATALOG_VERSION=1`，组件身份由编译目录限定。登记协议 V2
与签名快照协议 1、镜像标签 `com.basic-platform.license.protocol=1` 是不同版本概念，
不能据镜像标签判断登记仍是 V1。平台仍通过独立发布完整性/批准证据核验实际镜像；
组件更新不以复制旧 coverage/image 绑定或普通业务配置绕过受控发布。

共享 consumer 仍能读取未指定登记版本的 V1 绑定，用于已有组件兼容；受控新交付
不能据此回退。`COMMERCIAL_LICENSE_ENABLED=false` 只允许从未登记且没有其他绑定配置、
固定持久状态目录不存在或为空的组件；已有状态或绑定残留时会拒绝启动，不会因环境
文件丢失而静默放开。不可通过删状态卷或修改开关解除既有执行保护。

目录权威为 `license-core/catalog/catalog.go`（工作区路径）；本仓库构建使用审核过的
`third_party/license-core/catalog/catalog.go` 镜像。组件目录/发布批准与原业务授权均须满足。
平台操作见[运行时协调与激活](../../sharedDocs/09_商业授权/03_运行时协调与激活说明.md)。


API and mixed Worker consumers bind to `settlement` in compiled code. Approved
Compose components receive independent `runtime/license-<service>.env` files,
read-only platform public keys and separate durable signed-state volumes.
Credentials and signing private keys must not enter images, logs or shared
application environment files. Disabled compatibility is allowed only for never-enrolled components without other
binding fields or persisted runtime state; enabled incomplete configuration fails.

API historical query/download routes and historical report generation are
explicitly classified, not inferred from HTTP method. Unknown entries and all
new business mutations require entitlement; existing authentication, CSRF and
permission checks are preserved.

The mixed Worker retains existing audit/notification delivery and historical
report export. New dunning scans, tax reconciliation and business outbox
destinations check entitlement before claiming/executing work and again before
external business calls. License pauses do not mark delivery successful or spend
business failure attempts; leased work becomes eligible again after its lease.

Signed snapshots remain locally usable during loss of the platform connection,
only until their verified absolute deadline. Restart does not reset expiry.
Privileged deployment configuration is trusted; this is not protection against
root altering binaries or deleting configuration.

Verify `sh scripts/license-core-sync.sh --check` before build. Release approval
requires actual immutable image configuration digests and a nonempty reviewed
`APP_VERSION`, not mutable tags or container labels supplied by an operator.
The Docker runtime declares protocol `com.basic-platform.license.protocol=1`.

## 操作步骤与验收

部署管理员受控登记 settlement-api/settlement-worker后，平台许可管理员导入同实例许可，在“运行时授权执行”选结算与开票管理并刷新/确认。成功须部署READY、ENFORCED、snapshot_current=true及完整两个成员当期ACK。到期无写入宽限，既有授权项存在且已生效/快照可信/原权限正常才保留历史读和导出。

结算页面报表按钮 → frontend settlement/api的两类POST导出 → internal/httpapi/api.go商业EXPORT_HISTORY及settlement.report.export → 校验Idempotency-Key、按租户/请求人创建PENDING任务 → reportexport Worker固定租户、逐任务领取/心跳续租、owner+generation发布栅栏、CSV和文件网关副作用持久补偿 → READY → 页面再点下载。任务ID、202或“后台生成”提示不等于文件成功；页面轮询30次后仍可稍后查询。成功须READY且下载真实CSV，检查文件状态与审计，下载失败不能把审计入队异常忽略。

混合Worker保留历史导出及既有PLATFORM_AUDIT/PLATFORM_NOTIFICATION投递；dunning、tax reconciliation和业务destination在claim、事务提交、外部调用前复核。授权暂停不把事件标DELIVERED，不消耗普通失败次数。失败查独立消费者/Worker、租约、文件网关required配置和真实审计，再重试已有任务/受控部署；不要为只读停止整个Worker。

依据：frontend SettlementView.vue/runReportExport及api → internal/httpapi/api.go/commercial_license.go → settlement_report_export_job/SQL → internal/reportexport/worker.go →下载审计；cmd/worker装配outbox、dunning、taxreconcile门禁。本次未生成/下载报表、连接业务库或测试。
