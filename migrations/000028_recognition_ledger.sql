-- 财务确认账本使用租户串行锁保护确认余额、冲回额度与幂等结果。
CREATE TABLE settlement_recognition_lock (
 tenant_id VARCHAR(64) COLLATE utf8mb4_bin NOT NULL PRIMARY KEY,
 created_at DATETIME(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE TABLE settlement_recognition (
 id CHAR(32) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) COLLATE utf8mb4_bin NOT NULL,
 organization_id VARCHAR(128) COLLATE utf8mb4_bin NOT NULL,
 identity_id VARCHAR(128) COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 status VARCHAR(16) NOT NULL,
 source_claim_id VARCHAR(128) COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 original_id CHAR(32) NOT NULL DEFAULT '',
 source_key CHAR(64) COLLATE utf8mb4_bin DEFAULT NULL,
 payload JSON NOT NULL,
 created_at DATETIME(3) NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 UNIQUE KEY uq_recognition_source(tenant_id,source_key),
 KEY ix_recognition_scope(tenant_id,organization_id,created_at,id),
 KEY ix_recognition_claim(tenant_id,source_claim_id,status),
 KEY ix_recognition_original(tenant_id,original_id,status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE settlement_recognition_event (
 id CHAR(32) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(64) COLLATE utf8mb4_bin NOT NULL,
 recognition_id CHAR(32) NOT NULL,
 version BIGINT UNSIGNED NOT NULL,
 actor_id VARCHAR(128) COLLATE utf8mb4_bin NOT NULL,
 action VARCHAR(32) NOT NULL,
 payload JSON NOT NULL,
 created_at DATETIME(3) NOT NULL,
 UNIQUE KEY uq_recognition_version(tenant_id,recognition_id,version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
