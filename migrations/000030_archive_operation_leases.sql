-- 外部网关调用使用持久代次；逐列存在性保护支持DDL中断后的迁移恢复。
SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='settlement_invoice_archive_effect' AND column_name='operation_token')=0,
  'ALTER TABLE settlement_invoice_archive_effect ADD COLUMN operation_token CHAR(32) NOT NULL DEFAULT ''''', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;

SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='settlement_invoice_archive_effect' AND column_name='operation_until')=0,
  'ALTER TABLE settlement_invoice_archive_effect ADD COLUMN operation_until DATETIME(3) NULL', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;

SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='settlement_invoice_archive_effect' AND column_name='recovering')=0,
  'ALTER TABLE settlement_invoice_archive_effect ADD COLUMN recovering BOOLEAN NOT NULL DEFAULT FALSE', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;

SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='settlement_receipt_archive_effect' AND column_name='operation_token')=0,
  'ALTER TABLE settlement_receipt_archive_effect ADD COLUMN operation_token CHAR(32) NOT NULL DEFAULT ''''', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;

SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='settlement_receipt_archive_effect' AND column_name='operation_until')=0,
  'ALTER TABLE settlement_receipt_archive_effect ADD COLUMN operation_until DATETIME(3) NULL', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;

SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='settlement_receipt_archive_effect' AND column_name='recovering')=0,
  'ALTER TABLE settlement_receipt_archive_effect ADD COLUMN recovering BOOLEAN NOT NULL DEFAULT FALSE', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;

SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='settlement_recognition_evidence' AND column_name='operation_token')=0,
  'ALTER TABLE settlement_recognition_evidence ADD COLUMN operation_token CHAR(32) NOT NULL DEFAULT ''''', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;

SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='settlement_recognition_evidence' AND column_name='operation_until')=0,
  'ALTER TABLE settlement_recognition_evidence ADD COLUMN operation_until DATETIME(3) NULL', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;

SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='settlement_recognition_evidence' AND column_name='recovering')=0,
  'ALTER TABLE settlement_recognition_evidence ADD COLUMN recovering BOOLEAN NOT NULL DEFAULT FALSE', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;
