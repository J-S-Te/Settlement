ALTER TABLE settlement_tax_invoice
  ADD COLUMN currency CHAR(3) NOT NULL DEFAULT 'CNY' AFTER invoice_type,
  ADD COLUMN provider_code VARCHAR(64) NOT NULL DEFAULT '' AFTER issued_by_channel,
  ADD COLUMN external_invoice_id VARCHAR(128) NULL AFTER provider_code,
  ADD COLUMN external_payload_hash BINARY(32) NULL AFTER external_invoice_id,
  ADD UNIQUE KEY uq_settlement_tax_invoice_external(tenant_id,provider_code,external_invoice_id);

ALTER TABLE settlement_invoice_issue_attempt
  ADD COLUMN provider_code VARCHAR(64) NOT NULL DEFAULT '' AFTER channel,
  ADD COLUMN command_event_id CHAR(32) NULL AFTER idempotency_key,
  ADD COLUMN external_operation_id VARCHAR(128) NULL AFTER external_invoice_id,
  ADD COLUMN tax_invoice_id CHAR(32) NULL AFTER external_operation_id,
  ADD COLUMN result_sequence BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER status,
  ADD COLUMN last_result_event_id VARCHAR(128) NOT NULL DEFAULT '' AFTER result_sequence,
  ADD COLUMN accepted_at DATETIME(3) NULL AFTER requested_at,
  ADD COLUMN last_result_at DATETIME(3) NULL AFTER responded_at,
  ADD COLUMN last_reconciled_at DATETIME(3) NULL AFTER last_result_at,
  ADD COLUMN next_reconcile_at DATETIME(3) NULL AFTER last_reconciled_at,
  ADD COLUMN unknown_since DATETIME(3) NULL AFTER next_reconcile_at,
  ADD COLUMN reconcile_attempt_count INT NOT NULL DEFAULT 0 AFTER unknown_since,
  ADD UNIQUE KEY uq_settlement_issue_attempt_no(tenant_id,invoice_request_id,attempt_no),
  ADD UNIQUE KEY uq_settlement_issue_external_operation(tenant_id,provider_code,external_operation_id),
  ADD KEY idx_settlement_issue_reconcile(tenant_id,status,next_reconcile_at),
  ADD CONSTRAINT fk_settlement_issue_tax_invoice FOREIGN KEY(tax_invoice_id) REFERENCES settlement_tax_invoice(id);

CREATE TABLE IF NOT EXISTS settlement_tax_invoice_item (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, tax_invoice_id CHAR(32) NOT NULL, line_no INT NOT NULL,
  item_name VARCHAR(255) NOT NULL, tax_classification_code VARCHAR(64) NOT NULL,
  specification VARCHAR(128) NOT NULL DEFAULT '', unit VARCHAR(32) NOT NULL DEFAULT '',
  quantity DECIMAL(18,6) NOT NULL, unit_price_excl_tax DECIMAL(18,6) NOT NULL,
  amount_excl_tax DECIMAL(18,2) NOT NULL, tax_rate DECIMAL(8,6) NOT NULL,
  tax_amount DECIMAL(18,2) NOT NULL, amount_incl_tax DECIMAL(18,2) NOT NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_tax_invoice_item_line(tenant_id,tax_invoice_id,line_no),
  CONSTRAINT fk_settlement_tax_invoice_item_invoice FOREIGN KEY(tax_invoice_id) REFERENCES settlement_tax_invoice(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_invoice_red_flush_attempt (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, red_flush_request_id CHAR(32) NOT NULL,
  original_invoice_id CHAR(32) NOT NULL, attempt_no INT NOT NULL, provider_code VARCHAR(64) NOT NULL,
  external_request_id VARCHAR(128) NOT NULL, external_operation_id VARCHAR(128) NULL,
  command_event_id CHAR(32) NULL, red_invoice_id CHAR(32) NULL, status VARCHAR(32) NOT NULL,
  result_sequence BIGINT UNSIGNED NOT NULL DEFAULT 0, last_result_event_id VARCHAR(128) NOT NULL DEFAULT '',
  requested_at DATETIME(3) NOT NULL, accepted_at DATETIME(3) NULL, responded_at DATETIME(3) NULL,
  last_reconciled_at DATETIME(3) NULL, next_reconcile_at DATETIME(3) NULL, unknown_since DATETIME(3) NULL,
  reconcile_attempt_count INT NOT NULL DEFAULT 0, failure_code VARCHAR(64) NOT NULL DEFAULT '', failure_message VARCHAR(500) NOT NULL DEFAULT '',
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_red_attempt_no(tenant_id,red_flush_request_id,attempt_no),
  UNIQUE KEY uq_settlement_red_external_request(tenant_id,external_request_id),
  UNIQUE KEY uq_settlement_red_external_operation(tenant_id,provider_code,external_operation_id),
  KEY idx_settlement_red_reconcile(tenant_id,status,next_reconcile_at),
  CONSTRAINT fk_settlement_red_attempt_request FOREIGN KEY(red_flush_request_id) REFERENCES settlement_invoice_red_flush_request(id),
  CONSTRAINT fk_settlement_red_attempt_original FOREIGN KEY(original_invoice_id) REFERENCES settlement_tax_invoice(id),
  CONSTRAINT fk_settlement_red_attempt_invoice FOREIGN KEY(red_invoice_id) REFERENCES settlement_tax_invoice(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE settlement_invoice_red_flush_relation
  ADD COLUMN red_flush_request_id CHAR(32) NULL AFTER tenant_id,
  ADD UNIQUE KEY uq_settlement_red_flush_relation_request(tenant_id,red_flush_request_id),
  ADD CONSTRAINT fk_settlement_red_relation_request FOREIGN KEY(red_flush_request_id) REFERENCES settlement_invoice_red_flush_request(id);

CREATE TABLE IF NOT EXISTS settlement_invoice_allocation_reversal (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, red_flush_request_id CHAR(32) NOT NULL,
  original_allocation_id CHAR(32) NOT NULL, reversed_amount DECIMAL(18,2) NOT NULL, created_at DATETIME(3) NOT NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_allocation_reversal(tenant_id,red_flush_request_id,original_allocation_id),
  CONSTRAINT fk_settlement_allocation_reversal_request FOREIGN KEY(red_flush_request_id) REFERENCES settlement_invoice_red_flush_request(id),
  CONSTRAINT fk_settlement_allocation_reversal_allocation FOREIGN KEY(original_allocation_id) REFERENCES settlement_invoice_request_allocation(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_tax_result_inbox (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, provider_code VARCHAR(64) NOT NULL,
  event_id VARCHAR(128) NOT NULL, event_type VARCHAR(64) NOT NULL, operation_type VARCHAR(32) NOT NULL,
  aggregate_id CHAR(32) NOT NULL, external_request_id VARCHAR(128) NOT NULL, external_operation_id VARCHAR(128) NULL,
  result_sequence BIGINT UNSIGNED NOT NULL, result_status VARCHAR(32) NOT NULL,
  payload_json JSON NOT NULL, payload_hash BINARY(32) NOT NULL, processing_status VARCHAR(32) NOT NULL,
  failure_code VARCHAR(64) NOT NULL DEFAULT '', failure_summary VARCHAR(500) NOT NULL DEFAULT '',
  occurred_at DATETIME(3) NOT NULL, received_at DATETIME(3) NOT NULL, processed_at DATETIME(3) NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_tax_result_event(tenant_id,provider_code,event_id),
  KEY idx_settlement_tax_result_processing(processing_status,received_at),
  KEY idx_settlement_tax_result_sequence(tenant_id,external_request_id,result_sequence)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
