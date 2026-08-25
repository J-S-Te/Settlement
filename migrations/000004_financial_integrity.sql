CREATE TABLE IF NOT EXISTS settlement_contract_stream (
  tenant_id VARCHAR(64) NOT NULL, source_application VARCHAR(64) NOT NULL, source_contract_id VARCHAR(128) NOT NULL,
  current_version INT NOT NULL, updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY(tenant_id,source_application,source_contract_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_idempotency_record (
  tenant_id VARCHAR(64) NOT NULL, command_type VARCHAR(64) NOT NULL, idempotency_key VARCHAR(128) NOT NULL,
  request_hash BINARY(32) NOT NULL, resource_id VARCHAR(128) NOT NULL, response_json JSON NOT NULL, created_at DATETIME(3) NOT NULL,
  PRIMARY KEY(tenant_id,command_type,idempotency_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE settlement_receivable
  ADD CONSTRAINT chk_settlement_receivable_amounts CHECK(original_amount>0 AND invoiced_amount>=0 AND received_allocated_amount>=0 AND write_off_amount>=0 AND open_amount>=0);
ALTER TABLE settlement_receipt
  ADD CONSTRAINT chk_settlement_receipt_amounts CHECK(amount>0 AND unallocated_amount>=0 AND unallocated_amount<=amount);
ALTER TABLE settlement_receipt_allocation
  ADD CONSTRAINT chk_settlement_allocation_amount CHECK(allocated_amount>0);
ALTER TABLE settlement_receipt_allocation_reversal
  ADD CONSTRAINT chk_settlement_reversal_amount CHECK(reversed_amount>0);
ALTER TABLE settlement_invoice_request
  ADD CONSTRAINT chk_settlement_invoice_totals CHECK(amount_excl_tax>=0 AND tax_amount>=0 AND amount_incl_tax=amount_excl_tax+tax_amount);
