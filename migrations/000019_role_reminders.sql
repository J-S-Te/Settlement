ALTER TABLE settlement_dunning_case MODIFY receivable_id CHAR(32) NULL,
 ADD COLUMN plan_id CHAR(32) NULL,
 ADD UNIQUE KEY uq_dunning_plan_policy(tenant_id,plan_id,dunning_policy_id,policy_version),
 ADD CONSTRAINT fk_dunning_plan FOREIGN KEY(plan_id) REFERENCES settlement_receivable_plan(id);
CREATE TABLE settlement_role_reminder_cursor (
 tenant_id VARCHAR(64) NOT NULL,plan_id CHAR(32) NOT NULL,phase VARCHAR(32) NOT NULL,
 owner_user_id VARCHAR(128) NOT NULL,next_at DATETIME(3) NOT NULL,updated_at DATETIME(3) NOT NULL,
 PRIMARY KEY(tenant_id,plan_id,phase),KEY ix_role_reminder_due(tenant_id,next_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
