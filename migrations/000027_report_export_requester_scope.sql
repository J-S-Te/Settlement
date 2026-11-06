-- M-5：报表导出任务的申请人授权范围快照。
-- 报表读取/导出入口只对全范围主体放行（settlement.report.read / settlement.report.export），
-- 异步 worker 按任务行生成、不再持有请求主体，因此创建任务时必须把申请人范围固化到本列，
-- 避免旧任务、在飞任务或申请人后续缩权后仍按租户全量生成 CSV。
-- 历史行保持 NULL：没有快照就无法证明创建时的范围，worker 失败关闭（置 FAILED 并给出可读原因），
-- 由申请人重新发起导出；无需回填。
-- 回滚：先回滚读取 requester_scope 的二进制，再 DROP COLUMN requester_scope。
ALTER TABLE settlement_report_export_job
  ADD COLUMN requester_scope JSON NULL AFTER requested_by;
