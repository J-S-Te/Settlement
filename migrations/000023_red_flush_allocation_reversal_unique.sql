-- 红冲反冲销幂等约束（BIZ-SET-01）：同一原核销记录只允许被冲减一次。
-- 既有唯一键 uq_settlement_allocation_reversal(tenant_id,red_flush_request_id,original_allocation_id)
-- 只覆盖"同一红冲申请内不重复"，无法阻止同一原票的第二笔红冲再次冲减同一 original_allocation_id。
-- 本迁移把不变量提升到核销维度；应用层（reverseInvoiceAllocations）已先做同维度检查。
-- 失败关闭说明：若存量数据已存在同一（tenant_id,original_allocation_id）的重复冲减行，
-- ALTER 会以重复键错误中止整个迁移，不会自动删除或合并任何财务事实；
-- 此时须先人工核对下列查询结果并完成账务更正，再重新执行迁移：
--   SELECT tenant_id,original_allocation_id,COUNT(*) FROM settlement_invoice_allocation_reversal
--   GROUP BY tenant_id,original_allocation_id HAVING COUNT(*)>1;
ALTER TABLE settlement_invoice_allocation_reversal
  ADD UNIQUE KEY uq_settlement_allocation_reversal_allocation(tenant_id,original_allocation_id);
