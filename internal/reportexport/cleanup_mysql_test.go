package reportexport

import (
	"context"
	"testing"
	"time"
)

// TestMySQLExpiredExportContentPurge 验证过期导出的字节被清空且状态与过期时间不变，
// 未过期任务、其它租户以及未发布效果行都不受影响。
// 参数：t为测试上下文，仅使用显式隔离MySQL。
// 返回值：无，任一清理范围越界或状态被改写即失败。
func TestMySQLExpiredExportContentPurge(t *testing.T) {
	db := exportMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	insertJob := func(id, tenant, status string, content any, expiresSQL string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO settlement_report_export_job(id,tenant_id,requested_by,idempotency_key,report_type,status,file_content,expires_at,created_at,updated_at) VALUES(?,?,'user',?,? ,?,?,`+expiresSQL+`,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, id, tenant, id, InvoicedReceivablesCSV, status, content); err != nil {
			t.Fatalf("insert job %s: %v", id, err)
		}
	}
	insertEffect := func(jobID, tenant string, generation int, state string, content []byte) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO settlement_report_export_effect(job_id,lease_generation,tenant_id,application_id,request_id,file_name,file_content,state,created_at,updated_at) VALUES(?,?,?,'app',?, 'f.csv',?,? ,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, jobID, generation, tenant, jobID+"-"+state+"-req", content, state); err != nil {
			t.Fatalf("insert effect %s: %v", jobID, err)
		}
	}
	insertJob("expired", "tenant", "READY", []byte("expired-content"), "DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 DAY)")
	insertJob("expired-null", "tenant", "READY", nil, "DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 DAY)")
	insertJob("fresh", "tenant", "READY", []byte("fresh-content"), "DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 1 DAY)")
	insertJob("no-expiry", "tenant", "READY", []byte("legacy-content"), "NULL")
	insertJob("other-tenant", "tenant-other", "READY", []byte("other-content"), "DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 DAY)")
	insertEffect("expired", "tenant", 1, "PUBLISHED", []byte("published-effect"))
	insertEffect("expired", "tenant", 2, "PREPARED", []byte("pending-effect"))
	insertEffect("fresh", "tenant", 1, "PUBLISHED", []byte("fresh-effect"))
	insertEffect("no-expiry", "tenant", 1, "PUBLISHED", []byte("legacy-effect"))
	insertEffect("other-tenant", "tenant-other", 1, "PUBLISHED", []byte("other-effect"))

	worker := &Worker{DB: db, WorkerID: "worker-1", TenantID: "tenant"}
	if err := worker.purgeExpiredContent(ctx); err != nil {
		t.Fatal(err)
	}

	var status string
	var content []byte
	var expiresAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT status,file_content,expires_at FROM settlement_report_export_job WHERE id='expired'`).Scan(&status, &content, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if status != "READY" || content != nil || !expiresAt.Before(time.Now().UTC()) {
		t.Fatalf("expired job status=%q content=%q expires=%v", status, content, expiresAt)
	}
	// 下载契约仍在：过期任务即使字节已清空也保持 READY 和原过期时间，
	// HTTP 层因此继续返回 410，而不是 409。
	for id, wantStatus := range map[string]string{"expired-null": "READY", "fresh": "READY", "no-expiry": "READY", "other-tenant": "READY"} {
		if err := db.QueryRowContext(ctx, `SELECT status FROM settlement_report_export_job WHERE id=?`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != wantStatus {
			t.Fatalf("job %s status=%q want %q", id, status, wantStatus)
		}
	}
	var freshContent []byte
	if err := db.QueryRowContext(ctx, `SELECT file_content FROM settlement_report_export_job WHERE id='fresh'`).Scan(&freshContent); err != nil {
		t.Fatal(err)
	}
	if string(freshContent) != "fresh-content" {
		t.Fatalf("unexpired content=%q", freshContent)
	}
	var otherContent []byte
	if err := db.QueryRowContext(ctx, `SELECT file_content FROM settlement_report_export_job WHERE id='other-tenant'`).Scan(&otherContent); err != nil {
		t.Fatal(err)
	}
	if string(otherContent) != "other-content" {
		t.Fatalf("other tenant content=%q", otherContent)
	}
	effectContent := func(jobID string, generation int) []byte {
		t.Helper()
		var value []byte
		if err := db.QueryRowContext(ctx, `SELECT file_content FROM settlement_report_export_effect WHERE job_id=? AND lease_generation=?`, jobID, generation).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := effectContent("expired", 1); got != nil {
		t.Fatalf("published effect of expired job kept %q", got)
	}
	if got := string(effectContent("expired", 2)); got != "pending-effect" {
		t.Fatalf("unpublished effect must keep compensation bytes, got %q", got)
	}
	if got := string(effectContent("fresh", 1)); got != "fresh-effect" {
		t.Fatalf("published effect of unexpired job changed to %q", got)
	}
	if got := string(effectContent("no-expiry", 1)); got != "legacy-effect" {
		t.Fatalf("published effect without expiry changed to %q", got)
	}
	if got := string(effectContent("other-tenant", 1)); got != "other-effect" {
		t.Fatalf("other tenant effect changed to %q", got)
	}
}

// TestMySQLRunOncePurgesExpiredContent 验证每轮 RunOnce 在认领前执行过期清理，
// 且没有待处理任务时清理不制造错误。
// 参数：t为测试上下文，仅使用显式隔离MySQL。
// 返回值：无，RunOnce 未清理或返回错误即失败。
func TestMySQLRunOncePurgesExpiredContent(t *testing.T) {
	db := exportMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `INSERT INTO settlement_report_export_job(id,tenant_id,requested_by,idempotency_key,report_type,status,file_content,expires_at,created_at,updated_at) VALUES('expired','tenant','user','expired',?,'READY',? ,DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 DAY),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, InvoicedReceivablesCSV, []byte("expired-content")); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{DB: db, WorkerID: "worker-1", TenantID: "tenant", BatchSize: 1}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var content []byte
	if err := db.QueryRowContext(ctx, `SELECT status,file_content FROM settlement_report_export_job WHERE id='expired'`).Scan(&status, &content); err != nil {
		t.Fatal(err)
	}
	if status != "READY" || content != nil {
		t.Fatalf("status=%q content=%q", status, content)
	}
}
