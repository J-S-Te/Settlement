CREATE TABLE settlement_contract_responsibility (
 tenant_id VARCHAR(64) NOT NULL, source_contract_id VARCHAR(128) NOT NULL,
 owner_user_id VARCHAR(128) NOT NULL DEFAULT '', version BIGINT UNSIGNED NOT NULL,
 active BOOLEAN NOT NULL, updated_at DATETIME(3) NOT NULL,
 PRIMARY KEY(tenant_id,source_contract_id),
 KEY idx_settlement_contract_owner(tenant_id,owner_user_id,active,source_contract_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE settlement_contract_responsibility_inbox (
 tenant_id VARCHAR(64) NOT NULL, event_id VARCHAR(128) NOT NULL,
 payload_hash BINARY(32) NOT NULL, received_at DATETIME(3) NOT NULL,
 PRIMARY KEY(tenant_id,event_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE settlement_project_reference (
 tenant_id VARCHAR(64) NOT NULL, project_id VARCHAR(128) NOT NULL,
 source_contract_id VARCHAR(128) NOT NULL, customer_id VARCHAR(128) NOT NULL,
 project_name VARCHAR(255) NOT NULL DEFAULT '', version BIGINT UNSIGNED NOT NULL,
 active BOOLEAN NOT NULL, updated_at DATETIME(3) NOT NULL,
 PRIMARY KEY(tenant_id,project_id),
 KEY idx_settlement_project_contract(tenant_id,source_contract_id,active,project_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE settlement_project_assignment (
 tenant_id VARCHAR(64) NOT NULL, project_id VARCHAR(128) NOT NULL,
 user_id VARCHAR(128) NOT NULL, version BIGINT UNSIGNED NOT NULL,
 active BOOLEAN NOT NULL, note VARCHAR(500) NOT NULL DEFAULT '',
 updated_at DATETIME(3) NOT NULL, updated_by VARCHAR(128) NOT NULL,
 PRIMARY KEY(tenant_id,project_id,user_id),
 KEY idx_settlement_assistant(tenant_id,user_id,active,project_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
