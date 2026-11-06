package service

import (
	"context"
	"testing"
)

// TestImportExportExpiryRetentionOnMySQL 验证暂存对账导出超过 7 天后清空内容并标记 EXPIRED，
// 查询与创建都会触发清理，未到期任务、其它租户及非成功任务不受影响。
// 参数：t为测试上下文，仅使用显式隔离MySQL。
// 返回值：无，清理范围、状态或内容不符即失败。
func TestImportExportExpiryRetentionOnMySQL(t *testing.T) {
	db, _ := invoiceArchiveMySQL(t)
	ctx := context.Background()
	s := &Service{DB: db, ContractImportEnabled: true}
	in := importCommandFixture()
	in.ExpectedFacts = []ExpectedImportFact{}
	prepared, err := s.PrepareContractImport(ctx, in.TenantID, in)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := prepared.ContractSnapshotID
	p := Principal{TenantID: in.TenantID, UserID: "finance", Permissions: map[string]bool{"settlement.admin": true}}
	stale, err := s.CreateImportExport(ctx, p, snapshot, "stale-export")
	if err != nil || stale.Status != "SUCCEEDED" {
		t.Fatalf("stale export=%+v err=%v", stale, err)
	}
	fresh, err := s.CreateImportExport(ctx, p, snapshot, "fresh-export")
	if err != nil || fresh.Status != "SUCCEEDED" {
		t.Fatalf("fresh export=%+v err=%v", fresh, err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE settlement_import_export SET created_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 8 DAY) WHERE tenant_id=? AND id=?`, p.TenantID, stale.ID); err != nil {
		t.Fatal(err)
	}
	// 其它租户的到期行与失败行都不属于本次清理范围，保留原状态与字节。
	otherID, failedID := newID(), newID()
	if _, err = db.ExecContext(ctx, `INSERT INTO settlement_import_export(id,tenant_id,contract_snapshot_id,requested_by,status,snapshot_version,row_count,receipt_total,allocation_total,content,error_code,created_at) VALUES(?,?,'other-snapshot','other','SUCCEEDED',1,1,0,0,? ,'',DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 30 DAY))`, otherID, "import-other", []byte("other-content")); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO settlement_import_export(id,tenant_id,contract_snapshot_id,requested_by,status,snapshot_version,row_count,receipt_total,allocation_total,content,error_code,created_at) VALUES(?,?,?,'finance','FAILED',0,0,0,0,NULL,'RECONCILIATION_EXPORT_FAILED',DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 30 DAY))`, failedID, p.TenantID, snapshot); err != nil {
		t.Fatal(err)
	}

	// 查询触发清理：到期任务变为 EXPIRED 且不再返回内容（HTTP 层据非 SUCCEEDED 返回 409）。
	got, err := s.GetImportExport(ctx, p, snapshot, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "EXPIRED" || got.ErrorCode != "EXPORT_EXPIRED" || len(got.Content) != 0 {
		t.Fatalf("expired export=%+v", got)
	}
	var status string
	var content []byte
	if err = db.QueryRowContext(ctx, `SELECT status,content FROM settlement_import_export WHERE tenant_id=? AND id=?`, p.TenantID, stale.ID).Scan(&status, &content); err != nil {
		t.Fatal(err)
	}
	if status != "EXPIRED" || content != nil {
		t.Fatalf("stored expired status=%q content=%q", status, content)
	}
	// 未到期成功导出仍可下载。
	kept, err := s.GetImportExport(ctx, p, snapshot, fresh.ID)
	if err != nil || kept.Status != "SUCCEEDED" || len(kept.Content) == 0 {
		t.Fatalf("fresh export=%+v err=%v", kept, err)
	}
	var otherStatus string
	var otherContent []byte
	if err = db.QueryRowContext(ctx, `SELECT status,content FROM settlement_import_export WHERE id=?`, otherID).Scan(&otherStatus, &otherContent); err != nil {
		t.Fatal(err)
	}
	if otherStatus != "SUCCEEDED" || string(otherContent) != "other-content" {
		t.Fatalf("other tenant status=%q content=%q", otherStatus, otherContent)
	}
	var failedStatus string
	if err = db.QueryRowContext(ctx, `SELECT status FROM settlement_import_export WHERE id=?`, failedID).Scan(&failedStatus); err != nil {
		t.Fatal(err)
	}
	if failedStatus != "FAILED" {
		t.Fatalf("failed export status=%q", failedStatus)
	}

	// 创建触发清理：把在建任务改回到期后，下一次创建先清理它，再正常生成新导出。
	if _, err = db.ExecContext(ctx, `UPDATE settlement_import_export SET created_at=DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 8 DAY) WHERE tenant_id=? AND id=?`, p.TenantID, fresh.ID); err != nil {
		t.Fatal(err)
	}
	newer, err := s.CreateImportExport(ctx, p, snapshot, "after-expiry")
	if err != nil || newer.Status != "SUCCEEDED" {
		t.Fatalf("new export=%+v err=%v", newer, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT status,content FROM settlement_import_export WHERE id=?`, fresh.ID).Scan(&status, &content); err != nil {
		t.Fatal(err)
	}
	if status != "EXPIRED" || content != nil {
		t.Fatalf("create-triggered expiry status=%q content=%q", status, content)
	}
}
