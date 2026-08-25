# 结算与开票管理子系统 — 实施级逻辑 ER 图

> 这是结算域自己的逻辑数据模型，而不是把 CRM、合同、项目的数据库表复制进来。客户、项目、合同只以**来源标识 + 不可变业务快照**进入本系统；不得建立跨系统数据库外键。
>
> 所有金额使用 `DECIMAL` 定点数；所有核心表均应包含 `tenant_id`、创建/更新时间、操作者与乐观锁版本等通用字段。图中为突出领域关系，省略了这些重复字段。

> 维护说明：本图描述领域核心关系；`settlement_inbox_event`、`settlement_outbox_event`、`settlement_idempotency_record`、OIDC 会话、审计、通知和 `settlement_report_export_job` 是可靠性/认证/运营支撑表，必须以 `Settlement/migrations/` 的实际 DDL 为准。图中保留的 `TAX_INVOICE_ITEM` 代表设计预留，若迁移尚未创建该表，不得视为已上线能力。

```mermaid
erDiagram
    SETTLEMENT_CONTRACT_SNAPSHOT ||--o{ RECEIVABLE_PLAN : generates
    SETTLEMENT_CONTRACT_SNAPSHOT ||--o{ INVOICE_REQUEST : supports
    RECEIVABLE_PLAN ||--o| RECEIVABLE : confirms_to

    RECEIVABLE ||--o{ INVOICE_REQUEST_ALLOCATION : reserved_by
    INVOICE_REQUEST ||--o{ INVOICE_REQUEST_ITEM : contains
    INVOICE_REQUEST ||--o{ INVOICE_REQUEST_ALLOCATION : reserves
    INVOICE_REQUEST ||--o{ INVOICE_ISSUE_ATTEMPT : attempts
    INVOICE_REQUEST ||--o{ TAX_INVOICE : results_in

    TAX_INVOICE ||--o{ TAX_INVOICE_ITEM : contains
    TAX_INVOICE ||--o{ INVOICE_DOCUMENT : archives
    TAX_INVOICE ||--o{ INVOICE_RED_FLUSH_RELATION : original_of
    TAX_INVOICE ||--o{ INVOICE_RED_FLUSH_RELATION : red_of

    RECEIPT ||--o{ RECEIPT_ALLOCATION : distributes
    RECEIVABLE ||--o{ RECEIPT_ALLOCATION : settled_by
    RECEIPT_ALLOCATION ||--o{ RECEIPT_ALLOCATION_REVERSAL : reversed_by

    RECEIVABLE ||--o{ DUNNING_CASE : has
    DUNNING_POLICY ||--o{ DUNNING_CASE : governs
    DUNNING_CASE ||--o{ DUNNING_ACTION : records

    SETTLEMENT_CONTRACT_SNAPSHOT {
        string id PK
        string tenant_id
        string source_system
        string source_contract_id
        string source_contract_no
        int source_contract_version
        string customer_id
        string customer_name_snapshot
        string project_id
        string project_name_snapshot
        string currency
        decimal contract_amount
        string financial_status
        datetime effective_at
    }

    RECEIVABLE_PLAN {
        string id PK
        string contract_snapshot_id FK
        int installment_no
        date due_date
        decimal planned_amount
        string generation_source
        string confirmation_status
        string confirmation_reason
        int version
    }

    RECEIVABLE {
        string id PK
        string receivable_no UK
        string tenant_id
        string contract_snapshot_id FK
        string receivable_plan_id FK
        date due_date
        decimal original_amount
        decimal invoiced_amount
        decimal received_allocated_amount
        decimal write_off_amount
        decimal open_amount
        string currency
        string recognition_status
        string collection_status
        string invoice_status
        int version
    }

    INVOICE_REQUEST {
        string id PK
        string request_no UK
        string tenant_id
        string contract_snapshot_id FK
        json buyer_profile_snapshot
        string invoice_type
        decimal amount_excl_tax
        decimal tax_amount
        decimal amount_incl_tax
        string status
        string idempotency_key UK
        string submitted_by
        string approved_by
    }

    INVOICE_REQUEST_ITEM {
        string id PK
        string invoice_request_id FK
        int line_no
        string item_name
        string tax_classification_code
        decimal quantity
        decimal unit_price_excl_tax
        decimal amount_excl_tax
        decimal tax_rate
        decimal tax_amount
        decimal amount_incl_tax
    }

    INVOICE_REQUEST_ALLOCATION {
        string id PK
        string invoice_request_id FK
        string receivable_id FK
        decimal reserved_amount
        decimal invoiced_amount
        decimal released_amount
        string status
    }

    INVOICE_ISSUE_ATTEMPT {
        string id PK
        string invoice_request_id FK
        int attempt_no
        string channel
        string external_request_id UK
        string external_invoice_id
        string idempotency_key UK
        string status
        datetime requested_at
        datetime responded_at
        string failure_code
        datetime retry_after
    }

    TAX_INVOICE {
        string id PK
        string tenant_id
        string invoice_request_id FK
        string invoice_code
        string invoice_no
        string invoice_type
        decimal amount_excl_tax
        decimal tax_amount
        decimal amount_incl_tax
        date issue_date
        string status
        string issued_by_channel
    }

    TAX_INVOICE_ITEM {
        string id PK
        string tax_invoice_id FK
        int line_no
        string item_name
        decimal amount_excl_tax
        decimal tax_rate
        decimal tax_amount
        decimal amount_incl_tax
    }

    INVOICE_DOCUMENT {
        string id PK
        string tax_invoice_id FK
        string document_type
        string storage_object_id
        string checksum
        string access_classification
        datetime archived_at
    }

    INVOICE_RED_FLUSH_RELATION {
        string id PK
        string original_invoice_id FK
        string red_invoice_id FK
        string reason_code
        string reason_detail
        datetime created_at
    }

    RECEIPT {
        string id PK
        string receipt_no UK
        string tenant_id
        string customer_id
        string customer_name_snapshot
        decimal amount
        decimal unallocated_amount
        string currency
        date receipt_date
        string payment_method
        string bank_transaction_reference UK
        string source_type
        string status
    }

    RECEIPT_ALLOCATION {
        string id PK
        string allocation_no UK
        string tenant_id
        string receipt_id FK
        string receivable_id FK
        decimal allocated_amount
        string currency
        string status
        string match_mode
        decimal match_confidence
        string idempotency_key UK
        string confirmed_by
        datetime confirmed_at
    }

    RECEIPT_ALLOCATION_REVERSAL {
        string id PK
        string reversal_no UK
        string original_allocation_id FK
        decimal reversed_amount
        string reason_code
        string reason_detail
        string approved_by
        datetime reversed_at
    }

    DUNNING_POLICY {
        string id PK
        string tenant_id
        string name
        int version
        int aging_from_days
        int aging_to_days
        string action_type
        string recipient_rule
        string channel
        int repeat_interval_days
        boolean enabled
    }

    DUNNING_CASE {
        string id PK
        string receivable_id FK
        string dunning_policy_id FK
        int policy_version
        int current_escalation_level
        string status
        datetime next_action_at
        string closed_reason
    }

    DUNNING_ACTION {
        string id PK
        string dunning_case_id FK
        string action_type
        string recipient_user_id
        string channel
        string event_id UK
        string status
        datetime sent_at
        string result_detail
    }
```

## 关键建模规则

| 领域 | 实施规则 |
| --- | --- |
| 合同、客户、项目 | 通过来源 ID 回溯权威系统；结算单据保存名称、税务资料、付款条款等快照。上游变更不得覆盖已确认应收和已开票凭证。 |
| 应收 | `receivable_plan` 是计划，`receivable` 是已确认的财务义务；开票状态、收款状态、账龄必须分别表达。 |
| 开票 | 一张申请可有多行明细、占用多笔应收，并可产生多次开票尝试和一至多张税务发票。税控调用以 `invoice_issue_attempt` 的外部幂等键重试。 |
| 红冲/作废 | 不修改原发票。红票是新的 `tax_invoice`，并通过 `invoice_red_flush_relation` 连接原蓝票。 |
| 回款核销 | `receipt_allocation` 是最小不可变事实：一笔回款可分配多笔应收，反之亦然。错误以 `receipt_allocation_reversal` 冲销，不能更新或删除原分配记录。 |
| 并发控制 | 确认开票占用或回款分配时，必须在同一数据库事务中锁定相关应收和回款投影，校验余额不为负，并使用 `version` 作乐观锁保护。 |
| 催收 | 策略、案件、动作分离；`dunning_action.event_id` 是通知外发的幂等依据。普通提醒留在本系统；跨系统或高优先级提醒经 Outbox 异步投递基础平台。 |

## 数据库约束与索引（第一期必备）

```text
UNIQUE (tenant_id, source_system, source_contract_id, source_contract_version)
UNIQUE (tenant_id, source_event_id)                         -- 上游事件幂等
UNIQUE (tenant_id, request_no)
UNIQUE (tenant_id, invoice_request.idempotency_key)
UNIQUE (tenant_id, invoice_issue_attempt.external_request_id)
UNIQUE (tenant_id, receipt.bank_transaction_reference)      -- 银行流水去重
UNIQUE (tenant_id, receipt_allocation.idempotency_key)
UNIQUE (dunning_action.event_id)

INDEX  receivable(tenant_id, collection_status, due_date)
INDEX  receivable(tenant_id, customer_id, open_amount)
INDEX  receipt(tenant_id, status, receipt_date)
INDEX  invoice_request(tenant_id, status, created_at)
INDEX  dunning_case(tenant_id, status, next_action_at)
```

已确认的回款分配、冲销、发票及红冲记录只允许追加，禁止物理删除。涉及税号、银行账号、回单和发票文件的数据必须脱敏展示，并记录查看、下载、导出的审计事件。
