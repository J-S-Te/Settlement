-- Technical recovery journal. ARCHIVED is committed atomically with document
-- and audit; PENDING is the only state eligible for gateway compensation.
CREATE TABLE settlement_receipt_archive_effect (
  id CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  key_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  receipt_id CHAR(32) NOT NULL,
  document_type VARCHAR(64) NOT NULL,
  checksum CHAR(64) NOT NULL,
  original_name VARCHAR(255) NOT NULL,
  media_type VARCHAR(128) NOT NULL,
  application_id VARCHAR(128) NOT NULL,
  gateway_origin VARCHAR(512) NOT NULL,
  request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  file_id VARCHAR(128) NOT NULL DEFAULT '',
  document_id CHAR(32) NOT NULL DEFAULT '',
  state VARCHAR(16) NOT NULL,
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  available_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY(id),
  UNIQUE KEY uq_receipt_archive_key(tenant_id,key_hash),
  UNIQUE KEY uq_receipt_archive_request(request_id),
  KEY ix_receipt_archive_pending(tenant_id,state,available_at),
  KEY ix_receipt_archive_source(tenant_id,document_id,state,file_id,created_at),
  KEY ix_receipt_archive_content(tenant_id,receipt_id,document_type,checksum,state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE settlement_receipt_document (
 id CHAR(32) NOT NULL PRIMARY KEY,tenant_id VARCHAR(64) NOT NULL,receipt_id CHAR(32) NOT NULL,
 document_type VARCHAR(64) NOT NULL,original_name VARCHAR(255) NOT NULL,media_type VARCHAR(128) NOT NULL,
 storage_object_id VARCHAR(128) NOT NULL,checksum CHAR(64) NOT NULL,created_at DATETIME(3) NOT NULL,
 UNIQUE KEY uq_receipt_document(tenant_id,receipt_id,document_type,checksum)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
