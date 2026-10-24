// Package handover 实现人员离职交接的机器协议服务端语义（结算侧）。
// 结算责任仍由结算域持有：本包只负责冻结、逐项转移的校验与幂等，
// 并把真实写入交给仓储在本地事务内完成。
package handover

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrValidation = errors.New("handover request validation failed")
	ErrConflict   = errors.New("handover state conflict")
	ErrNotFound   = errors.New("handover record not found")
	ErrFrozen     = errors.New("user is frozen by an in-flight handover")
)

const (
	ReceiptApplied        = "APPLIED"
	ReceiptAlreadyApplied = "ALREADY_APPLIED"
	ReceiptRejected       = "REJECTED"
	ReceiptRetryable      = "RETRYABLE"

	FreezeFrozen   = "FROZEN"
	FreezeReleased = "RELEASED"
)

// 责任类别与资源类型：未完成的催收处理（催收动作的收件人）。
// 说明：催收策略的 recipient_rule 是规则而非用户，故策略本身不可归属到个人；
// 扫描器按规则解析出的催收动作收件人才是"人"的责任。
const (
	CategoryDunningRecipient = "DUNNING_ACTION_RECIPIENT"
	ResourceDunningAction    = "dunning_action"
)

type Responsibility struct {
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	ResourceName string    `json:"resource_name"`
	OwnerUserID  string    `json:"owner_user_id"`
	Category     string    `json:"category"`
	Version      uint64    `json:"version"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Freeze struct {
	ID, TenantID, BatchID, RequestID, UserID, Status string
	FrozenAt                                         time.Time
	ReleasedAt                                       *time.Time
	Outcome                                          string
}

type TransferInput struct {
	TenantID, BatchID, ItemID, RequestID, ResourceType, ResourceID string
	FromUserID, ToUserID                                           string
	ExpectedVersion                                                uint64
	IdempotencyKey                                                 string
}

type Receipt struct {
	ItemID, IdempotencyKey, Status string
	AppliedVersion                 uint64
	ReasonCode, EvidenceRef        string
	ReceivedAt                     time.Time
}

// Repository 是交接所需的最小持久化端口。实现必须在本地事务内完成责任变更与回执写入，
// 保证"回执存在 ⇒ 变更已生效"。
type Repository interface {
	CreateFreeze(context.Context, Freeze) (Freeze, bool, error)
	ReleaseFreeze(context.Context, string, string, string, time.Time) error
	ActiveFreeze(context.Context, string, string) (Freeze, bool, error)
	ListResponsibilities(context.Context, string, string, string, int) ([]Responsibility, error)
	TransferOwnership(context.Context, TransferInput, time.Time) (Receipt, error)
	FindReceipt(context.Context, string, string) (Receipt, bool, error)
}

type Service struct {
	repository Repository
	ids        func(time.Time) (string, error)
	now        func() time.Time
}

func NewService(repository Repository, ids func(time.Time) (string, error), now func() time.Time) (*Service, error) {
	if repository == nil || ids == nil || now == nil {
		return nil, errors.New("handover service dependencies must not be nil")
	}
	return &Service{repository: repository, ids: ids, now: now}, nil
}

// Freeze 持久化冻结（幂等），随后分页累计权威责任总数。
func (service *Service) Freeze(ctx context.Context, tenantID, batchID, requestID, userID string) (Freeze, int, error) {
	tenantID, batchID, requestID, userID = strings.TrimSpace(tenantID), strings.TrimSpace(batchID), strings.TrimSpace(requestID), strings.TrimSpace(userID)
	if tenantID == "" || batchID == "" || requestID == "" || userID == "" {
		return Freeze{}, 0, ErrValidation
	}
	now := service.now().UTC()
	freezeID, err := service.ids(now)
	if err != nil {
		return Freeze{}, 0, err
	}
	freeze, _, err := service.repository.CreateFreeze(ctx, Freeze{
		ID: freezeID, TenantID: tenantID, BatchID: batchID, RequestID: requestID, UserID: userID,
		Status: FreezeFrozen, FrozenAt: now,
	})
	if err != nil {
		return Freeze{}, 0, err
	}
	count, err := service.countResponsibilities(ctx, tenantID, userID)
	if err != nil {
		return Freeze{}, 0, err
	}
	return freeze, count, nil
}

func (service *Service) countResponsibilities(ctx context.Context, tenantID, userID string) (int, error) {
	total, cursor := 0, ""
	for {
		page, err := service.repository.ListResponsibilities(ctx, tenantID, userID, cursor, 200)
		if err != nil {
			return 0, err
		}
		total += len(page)
		if len(page) < 200 {
			return total, nil
		}
		cursor = page[len(page)-1].ResourceID
	}
}

// Responsibilities 返回分页权威责任清单。
func (service *Service) Responsibilities(ctx context.Context, tenantID, userID, cursor string, limit int) ([]Responsibility, string, error) {
	tenantID, userID = strings.TrimSpace(tenantID), strings.TrimSpace(userID)
	if tenantID == "" || userID == "" {
		return nil, "", ErrValidation
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	items, err := service.repository.ListResponsibilities(ctx, tenantID, userID, strings.TrimSpace(cursor), limit)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) == limit {
		next = items[len(items)-1].ResourceID
	}
	return items, next, nil
}

// Transfer 在本地事务内变更责任归属并写入幂等回执。
func (service *Service) Transfer(ctx context.Context, input TransferInput) (Receipt, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.BatchID = strings.TrimSpace(input.BatchID)
	input.ItemID = strings.TrimSpace(input.ItemID)
	input.ResourceType = strings.TrimSpace(input.ResourceType)
	input.ResourceID = strings.TrimSpace(input.ResourceID)
	input.FromUserID = strings.TrimSpace(input.FromUserID)
	input.ToUserID = strings.TrimSpace(input.ToUserID)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.TenantID == "" || input.BatchID == "" || input.ItemID == "" || input.ResourceID == "" ||
		input.FromUserID == "" || input.ToUserID == "" || input.IdempotencyKey == "" {
		return Receipt{}, ErrValidation
	}
	if input.FromUserID == input.ToUserID {
		return Receipt{}, fmt.Errorf("successor must differ from the current owner: %w", ErrValidation)
	}
	if existing, found, err := service.repository.FindReceipt(ctx, input.TenantID, input.IdempotencyKey); err != nil {
		return Receipt{}, err
	} else if found {
		return existing, nil
	}
	if freeze, found, err := service.repository.ActiveFreeze(ctx, input.TenantID, input.ToUserID); err != nil {
		return Receipt{}, err
	} else if found && freeze.Status == FreezeFrozen {
		return Receipt{}, fmt.Errorf("successor %s is frozen by handover %s: %w", input.ToUserID, freeze.BatchID, ErrFrozen)
	}
	return service.repository.TransferOwnership(ctx, input, service.now().UTC())
}

// Receipt 查询既有回执。
func (service *Service) Receipt(ctx context.Context, tenantID, idempotencyKey string) (Receipt, error) {
	receipt, found, err := service.repository.FindReceipt(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(idempotencyKey))
	if err != nil {
		return Receipt{}, err
	}
	if !found {
		return Receipt{}, ErrNotFound
	}
	return receipt, nil
}

// Complete 结束交接：解除冻结，已完成的转移保留。
func (service *Service) Complete(ctx context.Context, tenantID, batchID, outcome string) error {
	tenantID, batchID, outcome = strings.TrimSpace(tenantID), strings.TrimSpace(batchID), strings.ToUpper(strings.TrimSpace(outcome))
	if tenantID == "" || batchID == "" || (outcome != "COMPLETED" && outcome != "CANCELLED") {
		return ErrValidation
	}
	return service.repository.ReleaseFreeze(ctx, tenantID, batchID, outcome, service.now().UTC())
}
