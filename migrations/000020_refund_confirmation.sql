-- 退款确认仅补充外部完成事实，不改写历史金额或释放暂挂余额。
ALTER TABLE settlement_receipt_difference
 ADD COLUMN refund_reference VARCHAR(128) NOT NULL DEFAULT '',
 ADD COLUMN refund_completed_at DATETIME(3) NULL,
 ADD COLUMN refund_confirmed_by VARCHAR(128) NOT NULL DEFAULT '',
 ADD COLUMN refund_reason VARCHAR(500) NOT NULL DEFAULT '',
 ADD KEY ix_difference_browse (tenant_id,created_at DESC,id),
 ADD KEY ix_difference_status_browse (tenant_id,status,created_at DESC,id);

ALTER TABLE settlement_accounting_voucher
 ADD KEY ix_voucher_browse (tenant_id,created_at DESC,id),
 ADD KEY ix_voucher_status_browse (tenant_id,status,created_at DESC,id);
