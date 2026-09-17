ALTER TABLE settlement_invoice_document
  ADD COLUMN original_name VARCHAR(255) NOT NULL DEFAULT '' AFTER document_type,
  ADD COLUMN media_type VARCHAR(128) NOT NULL DEFAULT 'application/octet-stream' AFTER original_name;

ALTER TABLE settlement_receipt_allocation
  ADD COLUMN match_confidence INT NOT NULL DEFAULT 0 AFTER match_mode;

CREATE TABLE IF NOT EXISTS settlement_invoice_red_flush_request (
  id CHAR(32) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  original_invoice_id CHAR(32) NOT NULL,
  reason_code VARCHAR(64) NOT NULL,
  reason_detail VARCHAR(500) NOT NULL,
  idempotency_key VARCHAR(128) NOT NULL,
  status VARCHAR(32) NOT NULL,
  requested_by VARCHAR(128) NOT NULL,
  reviewed_by VARCHAR(128) NOT NULL DEFAULT '',
  review_reason VARCHAR(500) NOT NULL DEFAULT '',
  version INT NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY(id),
  UNIQUE KEY uq_settlement_red_flush_idempotency(tenant_id,idempotency_key),
  KEY idx_settlement_red_flush_request(tenant_id,original_invoice_id,status,created_at),
  CONSTRAINT fk_settlement_red_flush_request_invoice FOREIGN KEY(original_invoice_id) REFERENCES settlement_tax_invoice(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
