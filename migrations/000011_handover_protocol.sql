-- 人员离职交接机器协议（结算）：责任冻结与幂等回执。
-- 结算责任仍由结算域持有；本组表只记录"交接期间不得再分配新责任"与逐项转移回执。

CREATE TABLE IF NOT EXISTS settlement_handover_freeze (
  id CHAR(32) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  batch_id CHAR(32) NOT NULL,
  request_id CHAR(32) NOT NULL,
  user_id VARCHAR(128) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'FROZEN',
  frozen_at DATETIME(3) NOT NULL,
  released_at DATETIME(3) NULL,
  outcome VARCHAR(16) NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_settlement_handover_freeze_batch_user (tenant_id, batch_id, user_id),
  KEY idx_settlement_handover_freeze_active (tenant_id, user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_handover_receipt (
  id CHAR(32) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  batch_id CHAR(32) NOT NULL,
  item_id CHAR(32) NOT NULL,
  resource_type VARCHAR(64) NOT NULL,
  resource_id VARCHAR(128) NOT NULL,
  from_user_id VARCHAR(128) NOT NULL,
  to_user_id VARCHAR(128) NOT NULL,
  idempotency_key VARCHAR(160) NOT NULL,
  status VARCHAR(24) NOT NULL,
  applied_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
  reason_code VARCHAR(64) NULL,
  evidence_ref VARCHAR(255) NULL,
  received_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_settlement_handover_receipt_key (tenant_id, idempotency_key),
  KEY idx_settlement_handover_receipt_item (tenant_id, item_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
