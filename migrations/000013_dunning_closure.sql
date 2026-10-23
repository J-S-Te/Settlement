-- Dunning closure: persist action acknowledgement/version state and case timing.
ALTER TABLE settlement_dunning_case
  ADD COLUMN last_action_at DATETIME(3) NULL AFTER next_action_at,
  ADD COLUMN closed_at DATETIME(3) NULL AFTER closed_reason;

ALTER TABLE settlement_dunning_action
  ADD COLUMN version INT NOT NULL DEFAULT 1 AFTER status,
  ADD COLUMN updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) AFTER created_at,
  ADD COLUMN completed_at DATETIME(3) NULL AFTER sent_at,
  ADD COLUMN completed_by VARCHAR(128) NOT NULL DEFAULT '' AFTER completed_at,
  ADD KEY idx_settlement_dunning_action_pending (tenant_id, status, recipient_user_id);

-- Existing scanner actions already produced a local notification. Keep them
-- PENDING for human acknowledgement, but backfill the new audit timestamps so
-- the new UI/version guards treat them consistently. No action is auto-completed.
UPDATE settlement_dunning_action
SET updated_at = created_at,
    sent_at = COALESCE(sent_at, created_at),
    version = 1
WHERE version = 1;
