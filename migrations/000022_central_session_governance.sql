ALTER TABLE settlement_oidc_login_transaction
    ADD COLUMN central_ticket_ciphertext VARBINARY(1024) NULL;
ALTER TABLE settlement_oidc_session
    ADD COLUMN central_binding_ciphertext VARBINARY(1024) NULL,
    ADD COLUMN central_session_id VARCHAR(64) NULL,
    ADD COLUMN oidc_subject VARCHAR(256) NULL,
    ADD COLUMN oidc_sid VARCHAR(256) NULL,
    ADD KEY idx_settlement_oidc_sid (tenant_id,oidc_sid);
