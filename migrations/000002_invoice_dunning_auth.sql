CREATE TABLE IF NOT EXISTS settlement_oidc_login_transaction (
  state_hash BINARY(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL,
  nonce_ciphertext VARBINARY(512) NOT NULL, code_verifier_ciphertext VARBINARY(1024) NOT NULL,
  return_path VARCHAR(512) NOT NULL, expires_at DATETIME(3) NOT NULL, consumed_at DATETIME(3) NULL, created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (state_hash), KEY idx_settlement_oidc_login_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_oidc_session (
  session_id_hash BINARY(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, identity_id VARCHAR(128) NOT NULL,
  principal_json JSON NOT NULL, oauth_token_ciphertext MEDIUMBLOB NOT NULL, id_token_ciphertext MEDIUMBLOB NOT NULL,
  authorization_revision BIGINT UNSIGNED NOT NULL, authorization_checked_at DATETIME(3) NOT NULL,
  token_expires_at DATETIME(3) NOT NULL, session_expires_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL, last_seen_at DATETIME(3) NOT NULL, revoked_at DATETIME(3) NULL,
  PRIMARY KEY (session_id_hash), KEY idx_settlement_oidc_identity (tenant_id,identity_id), KEY idx_settlement_oidc_expiry (session_expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_invoice_request_item (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, invoice_request_id CHAR(32) NOT NULL, line_no INT NOT NULL,
  item_name VARCHAR(255) NOT NULL, tax_classification_code VARCHAR(64) NOT NULL, specification VARCHAR(128) NOT NULL DEFAULT '', unit VARCHAR(32) NOT NULL DEFAULT '',
  quantity DECIMAL(18,6) NOT NULL, unit_price_excl_tax DECIMAL(18,6) NOT NULL, amount_excl_tax DECIMAL(18,2) NOT NULL,
  tax_rate DECIMAL(8,6) NOT NULL, tax_amount DECIMAL(18,2) NOT NULL, amount_incl_tax DECIMAL(18,2) NOT NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_invoice_item_line(tenant_id,invoice_request_id,line_no),
  CONSTRAINT fk_settlement_invoice_item_request FOREIGN KEY(invoice_request_id) REFERENCES settlement_invoice_request(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_invoice_request_allocation (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, invoice_request_id CHAR(32) NOT NULL, receivable_id CHAR(32) NOT NULL,
  reserved_amount DECIMAL(18,2) NOT NULL, invoiced_amount DECIMAL(18,2) NOT NULL DEFAULT 0, released_amount DECIMAL(18,2) NOT NULL DEFAULT 0, status VARCHAR(32) NOT NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_invoice_allocation(tenant_id,invoice_request_id,receivable_id), KEY idx_settlement_invoice_allocation_receivable(tenant_id,receivable_id,status),
  CONSTRAINT fk_settlement_invoice_allocation_request FOREIGN KEY(invoice_request_id) REFERENCES settlement_invoice_request(id),
  CONSTRAINT fk_settlement_invoice_allocation_receivable FOREIGN KEY(receivable_id) REFERENCES settlement_receivable(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_invoice_issue_attempt (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, invoice_request_id CHAR(32) NOT NULL, attempt_no INT NOT NULL,
  channel VARCHAR(32) NOT NULL, external_request_id VARCHAR(128) NOT NULL, external_invoice_id VARCHAR(128) NOT NULL DEFAULT '', idempotency_key VARCHAR(128) NOT NULL,
  status VARCHAR(32) NOT NULL, requested_at DATETIME(3) NOT NULL, responded_at DATETIME(3) NULL, failure_code VARCHAR(64) NOT NULL DEFAULT '', failure_message VARCHAR(500) NOT NULL DEFAULT '', retry_after DATETIME(3) NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_issue_external(tenant_id,external_request_id), UNIQUE KEY uq_settlement_issue_idempotency(tenant_id,idempotency_key),
  CONSTRAINT fk_settlement_issue_request FOREIGN KEY(invoice_request_id) REFERENCES settlement_invoice_request(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_tax_invoice (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, invoice_request_id CHAR(32) NOT NULL,
  invoice_code VARCHAR(64) NOT NULL, invoice_no VARCHAR(64) NOT NULL, invoice_type VARCHAR(32) NOT NULL,
  amount_excl_tax DECIMAL(18,2) NOT NULL, tax_amount DECIMAL(18,2) NOT NULL, amount_incl_tax DECIMAL(18,2) NOT NULL,
  issue_date DATE NOT NULL, buyer_profile_snapshot JSON NOT NULL, seller_profile_snapshot JSON NOT NULL,
  status VARCHAR(32) NOT NULL, issued_by_channel VARCHAR(32) NOT NULL, created_at DATETIME(3) NOT NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_tax_invoice(tenant_id,invoice_code,invoice_no), KEY idx_settlement_tax_invoice_request(tenant_id,invoice_request_id),
  CONSTRAINT fk_settlement_tax_invoice_request FOREIGN KEY(invoice_request_id) REFERENCES settlement_invoice_request(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_invoice_document (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, tax_invoice_id CHAR(32) NOT NULL, document_type VARCHAR(32) NOT NULL,
  storage_object_id VARCHAR(128) NOT NULL, checksum VARCHAR(128) NOT NULL, access_classification VARCHAR(32) NOT NULL, archived_at DATETIME(3) NOT NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_invoice_document(tenant_id,tax_invoice_id,document_type,checksum),
  CONSTRAINT fk_settlement_invoice_document FOREIGN KEY(tax_invoice_id) REFERENCES settlement_tax_invoice(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_invoice_red_flush_relation (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, original_invoice_id CHAR(32) NOT NULL, red_invoice_id CHAR(32) NOT NULL,
  reason_code VARCHAR(64) NOT NULL, reason_detail VARCHAR(500) NOT NULL, created_by VARCHAR(128) NOT NULL, created_at DATETIME(3) NOT NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_red_invoice(tenant_id,red_invoice_id),
  CONSTRAINT fk_settlement_red_original FOREIGN KEY(original_invoice_id) REFERENCES settlement_tax_invoice(id),
  CONSTRAINT fk_settlement_red_invoice FOREIGN KEY(red_invoice_id) REFERENCES settlement_tax_invoice(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_dunning_policy (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, name VARCHAR(128) NOT NULL, version INT NOT NULL,
  aging_from_days INT NOT NULL, aging_to_days INT NOT NULL, action_type VARCHAR(32) NOT NULL, recipient_rule VARCHAR(128) NOT NULL,
  channel VARCHAR(32) NOT NULL, repeat_interval_days INT NOT NULL, priority VARCHAR(16) NOT NULL, enabled BOOLEAN NOT NULL,
  created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_dunning_policy(tenant_id,name,version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_dunning_case (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, receivable_id CHAR(32) NOT NULL, dunning_policy_id CHAR(32) NOT NULL, policy_version INT NOT NULL,
  current_escalation_level INT NOT NULL DEFAULT 0, status VARCHAR(32) NOT NULL, next_action_at DATETIME(3) NOT NULL, closed_reason VARCHAR(255) NOT NULL DEFAULT '', created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_dunning_case(tenant_id,receivable_id,dunning_policy_id,policy_version), KEY idx_settlement_dunning_due(tenant_id,status,next_action_at),
  CONSTRAINT fk_settlement_dunning_receivable FOREIGN KEY(receivable_id) REFERENCES settlement_receivable(id),
  CONSTRAINT fk_settlement_dunning_policy FOREIGN KEY(dunning_policy_id) REFERENCES settlement_dunning_policy(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_dunning_action (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, dunning_case_id CHAR(32) NOT NULL, action_type VARCHAR(32) NOT NULL,
  recipient_user_id VARCHAR(128) NOT NULL, channel VARCHAR(32) NOT NULL, priority VARCHAR(16) NOT NULL, event_id CHAR(32) NOT NULL,
  status VARCHAR(32) NOT NULL, sent_at DATETIME(3) NULL, result_detail VARCHAR(500) NOT NULL DEFAULT '', created_at DATETIME(3) NOT NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_dunning_event(tenant_id,event_id),
  CONSTRAINT fk_settlement_dunning_action_case FOREIGN KEY(dunning_case_id) REFERENCES settlement_dunning_case(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS settlement_audit_event (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, actor_id VARCHAR(128) NOT NULL, action VARCHAR(128) NOT NULL,
  resource_type VARCHAR(64) NOT NULL, resource_id VARCHAR(128) NOT NULL, result VARCHAR(16) NOT NULL, reason_code VARCHAR(64) NOT NULL,
  request_id VARCHAR(64) NOT NULL, correlation_id VARCHAR(64) NOT NULL, detail_json JSON NOT NULL, occurred_at DATETIME(3) NOT NULL,
  PRIMARY KEY(id), KEY idx_settlement_audit_resource(tenant_id,resource_type,resource_id,occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
