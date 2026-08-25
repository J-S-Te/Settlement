ALTER TABLE settlement_outbox_event
  ADD COLUMN locked_by VARCHAR(128) NULL AFTER available_at,
  ADD COLUMN locked_until DATETIME(3) NULL AFTER locked_by,
  ADD COLUMN last_error_code VARCHAR(128) NOT NULL DEFAULT '' AFTER delivered_at,
  ADD COLUMN dead_lettered_at DATETIME(3) NULL AFTER last_error_summary,
  ADD COLUMN updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) AFTER created_at,
  DROP INDEX uq_settlement_outbox_event,
  ADD UNIQUE KEY uq_settlement_outbox_destination_event (tenant_id,destination,event_id),
  ADD KEY idx_settlement_outbox_lease (destination,status,available_at,locked_until,created_at,id);
