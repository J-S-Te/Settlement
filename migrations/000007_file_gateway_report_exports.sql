ALTER TABLE settlement_report_export_job
  ADD COLUMN platform_file_id VARCHAR(64) NOT NULL DEFAULT '' AFTER file_name,
  ADD COLUMN platform_file_version BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER platform_file_id,
  ADD COLUMN platform_file_sha256 CHAR(64) NOT NULL DEFAULT '' AFTER platform_file_version,
  ADD COLUMN platform_file_size BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER platform_file_sha256,
  ADD COLUMN platform_file_status VARCHAR(16) NOT NULL DEFAULT 'DISABLED' AFTER platform_file_size,
  ADD KEY idx_settlement_report_platform_file (tenant_id, platform_file_status, created_at);
