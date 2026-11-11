CREATE TABLE settlement_recognition_evidence (
 id CHAR(32) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) COLLATE utf8mb4_bin NOT NULL,
 organization_id VARCHAR(128) COLLATE utf8mb4_bin NOT NULL,
 created_by VARCHAR(128) COLLATE utf8mb4_bin NOT NULL,
 name VARCHAR(255) NOT NULL,
 media_type VARCHAR(64) NOT NULL,
 checksum CHAR(64) COLLATE utf8mb4_bin NOT NULL,
 byte_size BIGINT NOT NULL,
 status VARCHAR(16) NOT NULL,
 application_id VARCHAR(128) COLLATE utf8mb4_bin NOT NULL,
 gateway_origin VARCHAR(512) COLLATE utf8mb4_bin NOT NULL,
 file_id VARCHAR(128) COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 created_at DATETIME(3) NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 KEY ix_recognition_evidence_scope(tenant_id,organization_id,status,created_at,id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
