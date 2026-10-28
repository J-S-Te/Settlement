CREATE TABLE settlement_settings (
 tenant_id VARCHAR(64) NOT NULL PRIMARY KEY,
 settings_json JSON NOT NULL,
 version INT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE settlement_invoice_special_review (
 tenant_id VARCHAR(64) NOT NULL,
 invoice_request_id CHAR(32) NOT NULL,
 special_reason VARCHAR(500) NOT NULL,
 threshold_snapshot VARCHAR(32) NOT NULL,
 settings_version INT NOT NULL,
 required_review BOOLEAN NOT NULL,
 status VARCHAR(32) NOT NULL,
 reviewed_by VARCHAR(128) NOT NULL DEFAULT '',
 review_reason VARCHAR(500) NOT NULL DEFAULT '',
 PRIMARY KEY(tenant_id,invoice_request_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE settlement_receipt_difference (
 id CHAR(32) NOT NULL PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,receipt_id CHAR(32) NOT NULL,
 kind VARCHAR(32) NOT NULL,amount DECIMAL(18,2) NOT NULL,reason VARCHAR(500) NOT NULL,
 status VARCHAR(32) NOT NULL,resolution VARCHAR(32) NOT NULL DEFAULT '',resolution_reason VARCHAR(500) NOT NULL DEFAULT '',
 requested_by VARCHAR(128) NOT NULL,resolved_by VARCHAR(128) NOT NULL DEFAULT '',approved_by VARCHAR(128) NOT NULL DEFAULT '',
 version INT NOT NULL DEFAULT 1,created_at DATETIME(3) NOT NULL,
 KEY ix_difference(tenant_id,status,created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE settlement_accounting_voucher (
 id CHAR(32) NOT NULL PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,writeoff_id CHAR(32) NOT NULL,
 payload JSON NOT NULL,status VARCHAR(32) NOT NULL,external_no VARCHAR(128) NOT NULL DEFAULT '',
 attempts INT NOT NULL DEFAULT 0,version INT NOT NULL DEFAULT 1,created_at DATETIME(3) NOT NULL,
 UNIQUE KEY uq_voucher_source(tenant_id,writeoff_id),KEY ix_voucher(tenant_id,status,created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
