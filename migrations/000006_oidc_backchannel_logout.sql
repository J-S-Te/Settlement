CREATE TABLE IF NOT EXISTS settlement_oidc_backchannel_logout_replay (
  jti_hash BINARY(32) NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (jti_hash),
  KEY idx_settlement_oidc_logout_replay_expiry (expires_at)
) ENGINE=InnoDB;
