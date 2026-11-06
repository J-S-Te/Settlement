-- Explicit local financial evidence, distinct from external contract identity.
CREATE TABLE settlement_financial_customer (
  id CHAR(32) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  name VARCHAR(255) NOT NULL,
  status VARCHAR(16) NOT NULL,
  created_by VARCHAR(128) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (tenant_id,id),
  KEY idx_financial_customer_name (tenant_id,status,name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE settlement_contract_snapshot
  MODIFY source_contract_id VARCHAR(128) NULL,
  ADD COLUMN basis_type VARCHAR(32) NOT NULL DEFAULT 'CONTRACT',
  ADD COLUMN customer_namespace VARCHAR(64) NOT NULL DEFAULT 'legacy',
  ADD COLUMN local_customer_id CHAR(32) NULL,
  ADD COLUMN local_document_id CHAR(32) NULL,
  ADD COLUMN local_document_no VARCHAR(128) NULL,
  ADD COLUMN local_document_version INT NULL,
  ADD COLUMN local_evidence_type VARCHAR(32) NULL,
  ADD COLUMN local_evidence_reference VARCHAR(200) NULL,
  ADD COLUMN local_evidence_note VARCHAR(1000) NULL,
  ADD COLUMN local_created_by VARCHAR(128) NULL,
  ADD COLUMN local_void_reason VARCHAR(500) NULL,
  ADD COLUMN local_voided_by VARCHAR(128) NULL,
  ADD COLUMN local_voided_at DATETIME(3) NULL,
  ADD UNIQUE KEY uq_local_financial_document (tenant_id,local_document_id,local_document_version),
  ADD UNIQUE KEY uq_local_financial_number (tenant_id,local_document_no),
  ADD CONSTRAINT fk_local_financial_customer FOREIGN KEY (tenant_id,local_customer_id)
    REFERENCES settlement_financial_customer(tenant_id,id),
  ADD CONSTRAINT chk_settlement_financial_basis CHECK (
    (basis_type='CONTRACT' AND source_contract_id IS NOT NULL AND local_document_id IS NULL)
    OR
    (basis_type='LOCAL_FINANCIAL' AND source_application='settlement_local'
      AND source_contract_id IS NULL AND source_contract_no='' AND source_contract_version=0
      AND local_document_id IS NOT NULL AND local_document_no IS NOT NULL
      AND local_document_version IS NOT NULL AND local_document_version>=1
      AND local_evidence_type IS NOT NULL AND local_evidence_reference IS NOT NULL
      AND local_evidence_note IS NOT NULL AND local_created_by IS NOT NULL
      AND local_customer_id IS NOT NULL AND customer_namespace='settlement_local'
      AND customer_id=CONCAT('settlement_local:',local_customer_id)
      AND customer_id LIKE 'settlement_local:%' AND contract_amount>0)
  );
-- Existing contract rows retain their identity, indexes and accounting facts.
-- Once local facts are written, recover forward; do not drop or relabel them.
