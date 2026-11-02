-- 合同财务修订人工复核工单（BIZ-XEVT2-05 结算侧）。
-- 结算侧接收高于当前版本的合同财务事件时，未实现部分可以自动重排；
-- 已经产生回款、核销、开票或发票预占的差异不得被自动改写，统一登记本表由财务人工修订。
-- 幂等：迁移器按文件名+校验和只执行一次；语句本身使用 IF NOT EXISTS，重复执行不会破坏既有工单。
-- 回填：无。旧数据不产生工单；旧二进制不读取本表，回滚代码即可。
-- 值域：hold_reason 取 PLAN_DIFF_IMPLEMENTED_FACTS / TERM_TOTAL_MISMATCH；
-- status 取 PENDING_REVIEW / RESOLVED。均为 VARCHAR 值域，后续扩展无需 DDL。
CREATE TABLE IF NOT EXISTS settlement_contract_revision_hold (
  id CHAR(32) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  source_application VARCHAR(64) NOT NULL,
  source_contract_id VARCHAR(128) NOT NULL,
  source_contract_no VARCHAR(128) NOT NULL,
  contract_snapshot_id CHAR(32) NULL,
  revision_event_id CHAR(32) NOT NULL,
  from_contract_version INT NOT NULL,
  to_contract_version INT NOT NULL,
  hold_reason VARCHAR(64) NOT NULL,
  diff_json JSON NOT NULL,
  status VARCHAR(32) NOT NULL,
  resolution_reason VARCHAR(500) NOT NULL DEFAULT '',
  resolved_by VARCHAR(128) NOT NULL DEFAULT '',
  resolved_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_settlement_contract_revision_event (tenant_id,source_application,revision_event_id,hold_reason),
  KEY idx_settlement_contract_revision_contract (tenant_id,source_contract_id,to_contract_version),
  KEY idx_settlement_contract_revision_status (tenant_id,status,created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
