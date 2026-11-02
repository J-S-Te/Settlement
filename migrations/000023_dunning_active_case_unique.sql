-- 催收唯一 ACTIVE 案件（BIZ-SET-07）：同一租户下同一应收或同一回款计划最多只能有一个 ACTIVE 催收案件。
-- MySQL 没有部分唯一索引，这里用 VIRTUAL 生成列把“只有 ACTIVE 参与唯一性”表达出来：
--   * ACTIVE 行生成列写入应收/计划业务键；
--   * 非 ACTIVE 行生成列为 NULL，而唯一索引不约束 NULL，历史 CLOSED 案件因此可以长期保留。
-- 应用层 scanner/role_reminder 的冲突校验只覆盖单 worker 时序；本迁移把同一不变量提升到数据库，
-- 关闭多 worker 并发窗口，防止同一催收义务产生第二套动作、通知和升级级别。
-- 幂等与失败关闭：迁移器按文件名+校验和只执行一次；若存量数据已存在同一应收或同一计划的多条
-- ACTIVE 案件，ALTER 会以 1062 重复键错误整体中止（MySQL 8 原子 DDL），不会自动删除、合并、
-- 改写或关闭任何案件。历史重复必须先由业务人工核对处理；本迁移不承担数据更正。
-- 迁移前置只读预检（必须在执行迁移前运行，两条查询都必须返回空集）：
--   SELECT tenant_id,receivable_id,COUNT(*) AS active_cases
--     FROM settlement_dunning_case
--    WHERE status='ACTIVE' AND receivable_id IS NOT NULL
--    GROUP BY tenant_id,receivable_id HAVING COUNT(*)>1;
--   SELECT tenant_id,plan_id,COUNT(*) AS active_cases
--     FROM settlement_dunning_case
--    WHERE status='ACTIVE' AND plan_id IS NOT NULL
--    GROUP BY tenant_id,plan_id HAVING COUNT(*)>1;
-- 预检非空时的处理：由业务确认哪一个是权威案件并关闭其余案件（保留案件与动作历史），
-- 再重新执行迁移；迁移本身不会自动挑选、关闭或删除任何案件。
ALTER TABLE settlement_dunning_case
  ADD COLUMN active_receivable_id CHAR(32)
    GENERATED ALWAYS AS (IF(status='ACTIVE',receivable_id,NULL)) VIRTUAL,
  ADD COLUMN active_plan_id CHAR(32)
    GENERATED ALWAYS AS (IF(status='ACTIVE',plan_id,NULL)) VIRTUAL,
  ADD UNIQUE KEY uq_settlement_dunning_active_receivable (tenant_id,active_receivable_id),
  ADD UNIQUE KEY uq_settlement_dunning_active_plan (tenant_id,active_plan_id);
