ALTER TABLE settlement_contract_snapshot
 ADD COLUMN migration_state VARCHAR(16) NOT NULL DEFAULT 'RELEASED',
 ADD COLUMN import_snapshot_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
 ADD KEY idx_settlement_migration_state (tenant_id,migration_state,id);
CREATE TABLE settlement_contract_import (
 tenant_id VARCHAR(64) NOT NULL,
 contract_snapshot_id CHAR(32) NOT NULL,
 origin_import_item_id VARCHAR(128) NOT NULL,
 source_revision BIGINT UNSIGNED NOT NULL,
 policy_digest CHAR(64) NOT NULL,
 release_version BIGINT UNSIGNED NOT NULL,
 import_mode VARCHAR(32) NOT NULL,
 owner_user_id VARCHAR(128) NOT NULL,
 prepare_event_id VARCHAR(128) NOT NULL,
 expected_facts_json JSON NOT NULL,
 state VARCHAR(24) NOT NULL DEFAULT 'PREPARED',
 reconciliation_id CHAR(32) NOT NULL DEFAULT '',
 credit_decision VARCHAR(16) NOT NULL DEFAULT '',
 credit_reason VARCHAR(500) NOT NULL DEFAULT '',
 approved_by VARCHAR(128) NOT NULL DEFAULT '',
 released_at DATETIME(3) NULL,
 PRIMARY KEY (tenant_id,contract_snapshot_id),
 UNIQUE KEY uq_import_item (tenant_id,origin_import_item_id),
 CONSTRAINT fk_import_snapshot FOREIGN KEY (contract_snapshot_id) REFERENCES settlement_contract_snapshot(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE settlement_import_fact (
 tenant_id VARCHAR(64) NOT NULL,
 contract_snapshot_id CHAR(32) NOT NULL,
 source_system VARBINARY(64) NOT NULL,
 fact_type VARCHAR(32) NOT NULL,
 external_fact_id VARBINARY(128) NOT NULL,
 request_hash BINARY(32) NOT NULL,
 target_business_id CHAR(32) NOT NULL,
 created_at DATETIME(3) NOT NULL,
 PRIMARY KEY (tenant_id,contract_snapshot_id,source_system,fact_type,external_fact_id),
 KEY idx_import_fact_target (tenant_id,target_business_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE settlement_import_reconciliation (
 id CHAR(32) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 contract_snapshot_id CHAR(32) NOT NULL,
 snapshot_version BIGINT UNSIGNED NOT NULL,
 balances_hash BINARY(32) NOT NULL,
 statement_json JSON NOT NULL,
 reconciled_by VARCHAR(128) NOT NULL,
 created_at DATETIME(3) NOT NULL,
 KEY idx_import_reconciliation (tenant_id,contract_snapshot_id,snapshot_version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE settlement_import_export (
 id CHAR(32) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) NOT NULL,
 contract_snapshot_id CHAR(32) NOT NULL,
 requested_by VARCHAR(128) NOT NULL,
 status VARCHAR(16) NOT NULL,
 snapshot_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
 row_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
 receipt_total DECIMAL(18,2) NOT NULL DEFAULT 0,
 allocation_total DECIMAL(18,2) NOT NULL DEFAULT 0,
 content LONGBLOB NULL,
 error_code VARCHAR(64) NOT NULL DEFAULT '',
 created_at DATETIME(3) NOT NULL,
 finished_at DATETIME(3) NULL,
 KEY idx_import_export (tenant_id,contract_snapshot_id,created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
