-- Settlement -> CRM credit sync: allow a completion event and a later reversal
-- event for the same receivable/allocation, and carry the canonical event type.
-- The original allocation/receivable uniqueness prevented a legitimate
-- settle -> reverse -> settle lifecycle from ever inserting the second event.
ALTER TABLE settlement_credit_sync
  DROP INDEX uq_settlement_credit_sync_receivable,
  DROP INDEX uq_settlement_credit_sync_allocation,
  ADD COLUMN event_type VARCHAR(64) NOT NULL DEFAULT 'customer.credit.payment.completed.v1' AFTER event_id,
  ADD COLUMN original_event_id CHAR(32) NOT NULL DEFAULT '' AFTER event_type,
  ADD KEY idx_settlement_credit_sync_allocation (tenant_id,allocation_id);

-- Rollback note: this repository's migration runner applies only forward
-- migrations. To roll back, first verify at most one event exists per
-- receivable/allocation, then drop idx_settlement_credit_sync_allocation,
-- original_event_id and event_type, and restore the old unique keys on
-- (tenant_id,receivable_id) and (tenant_id,allocation_id).
