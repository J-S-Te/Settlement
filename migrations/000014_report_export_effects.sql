ALTER TABLE settlement_report_export_job
  ADD COLUMN lease_generation BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER locked_until;

-- Durable intent is committed before any gateway request, including its bytes
-- so an unknown upload response can be resolved with the same request hash.
CREATE TABLE settlement_report_export_effect (
  job_id CHAR(32) NOT NULL,
  lease_generation BIGINT UNSIGNED NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  application_id VARCHAR(64) NOT NULL,
  request_id VARCHAR(128) NOT NULL,
  file_name VARCHAR(255) NOT NULL,
  file_content MEDIUMBLOB NULL,
  file_id VARCHAR(64) NOT NULL DEFAULT '',
  state VARCHAR(16) NOT NULL,
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  available_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (job_id, lease_generation),
  UNIQUE KEY uq_settlement_export_request (request_id),
  KEY idx_settlement_export_effect_retry (state, available_at, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
