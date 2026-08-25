CREATE TABLE IF NOT EXISTS settlement_local_notification (
  id CHAR(32) NOT NULL, tenant_id VARCHAR(64) NOT NULL, recipient_user_id VARCHAR(128) NOT NULL, event_id CHAR(32) NOT NULL,
  notification_scope VARCHAR(32) NOT NULL, priority VARCHAR(16) NOT NULL, title VARCHAR(500) NOT NULL, content TEXT NOT NULL,
  target_url VARCHAR(512) NOT NULL, reference_type VARCHAR(64) NOT NULL, reference_id VARCHAR(128) NOT NULL,
  created_at DATETIME(3) NOT NULL, read_at DATETIME(3) NULL,
  PRIMARY KEY(id), UNIQUE KEY uq_settlement_local_notification(tenant_id,recipient_user_id,event_id),
  KEY idx_settlement_notification_inbox(tenant_id,recipient_user_id,read_at,created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
