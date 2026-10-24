package handover

import (
	"context"
	"errors"
	"testing"
	"time"
)

type repositoryFake struct {
	freezes          map[string]Freeze
	responsibilities []Responsibility
	receipts         map[string]Receipt
	transferCalls    int
	released         []string
}

func newRepositoryFake(resources ...string) *repositoryFake {
	fake := &repositoryFake{freezes: map[string]Freeze{}, receipts: map[string]Receipt{}}
	for _, resource := range resources {
		fake.responsibilities = append(fake.responsibilities, Responsibility{
			ResourceType: ResourceDunningAction, ResourceID: resource, ResourceName: "催收动作-" + resource,
			OwnerUserID: "user-leaver", Category: CategoryDunningRecipient, Version: 1, UpdatedAt: time.Now().UTC(),
		})
	}
	return fake
}

func (fake *repositoryFake) CreateFreeze(_ context.Context, freeze Freeze) (Freeze, bool, error) {
	key := freeze.TenantID + "|" + freeze.BatchID + "|" + freeze.UserID
	if existing, ok := fake.freezes[key]; ok {
		return existing, false, nil
	}
	fake.freezes[key] = freeze
	return freeze, true, nil
}

func (fake *repositoryFake) ReleaseFreeze(_ context.Context, tenantID, batchID, outcome string, now time.Time) error {
	fake.released = append(fake.released, tenantID+"|"+batchID+"|"+outcome)
	for key, freeze := range fake.freezes {
		if freeze.BatchID == batchID {
			freeze.Status, freeze.ReleasedAt, freeze.Outcome = FreezeReleased, &now, outcome
			fake.freezes[key] = freeze
		}
	}
	return nil
}

func (fake *repositoryFake) ActiveFreeze(_ context.Context, tenantID, userID string) (Freeze, bool, error) {
	for _, freeze := range fake.freezes {
		if freeze.TenantID == tenantID && freeze.UserID == userID && freeze.Status == FreezeFrozen {
			return freeze, true, nil
		}
	}
	return Freeze{}, false, nil
}

func (fake *repositoryFake) ListResponsibilities(_ context.Context, _, userID, cursor string, limit int) ([]Responsibility, error) {
	items := []Responsibility{}
	for _, item := range fake.responsibilities {
		if item.OwnerUserID != userID || (cursor != "" && item.ResourceID <= cursor) {
			continue
		}
		items = append(items, item)
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

func (fake *repositoryFake) TransferOwnership(_ context.Context, input TransferInput, now time.Time) (Receipt, error) {
	fake.transferCalls++
	receipt := Receipt{ItemID: input.ItemID, IdempotencyKey: input.IdempotencyKey, Status: ReceiptApplied,
		AppliedVersion: input.ExpectedVersion + 1, EvidenceRef: "settlement:" + input.ResourceID, ReceivedAt: now}
	fake.receipts[input.TenantID+"|"+input.IdempotencyKey] = receipt
	return receipt, nil
}

func (fake *repositoryFake) FindReceipt(_ context.Context, tenantID, idempotencyKey string) (Receipt, bool, error) {
	receipt, ok := fake.receipts[tenantID+"|"+idempotencyKey]
	return receipt, ok, nil
}

func serviceFor(t *testing.T, repository *repositoryFake) *Service {
	t.Helper()
	counter := 0
	service, err := NewService(repository, func(time.Time) (string, error) {
		counter++
		return "01ST" + string(rune('A'+counter%26)) + "000000000000000000000", nil
	}, func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// 冻结幂等且报告权威责任总数。
func TestFreezeIsIdempotentAndReportsResponsibilities(t *testing.T) {
	repository := newRepositoryFake("act-1", "act-2", "act-3")
	service := serviceFor(t, repository)
	ctx := context.Background()

	first, count, err := service.Freeze(ctx, "tenant-1", "batch-1", "request-1", "user-leaver")
	if err != nil || first.Status != FreezeFrozen || count != 3 {
		t.Fatalf("freeze = %+v count=%d err=%v", first, count, err)
	}
	second, count, err := service.Freeze(ctx, "tenant-1", "batch-1", "request-1", "user-leaver")
	if err != nil || second.ID != first.ID || count != 3 {
		t.Fatalf("repeated freeze must be idempotent: %+v count=%d err=%v", second, count, err)
	}
	if _, _, err := service.Freeze(ctx, "tenant-1", "batch-1", "request-1", ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing user must be rejected: %v", err)
	}
}

// 清单分页且不泄露他人责任。
func TestResponsibilitiesPaginate(t *testing.T) {
	repository := newRepositoryFake("act-1", "act-2", "act-3")
	service := serviceFor(t, repository)

	items, next, err := service.Responsibilities(context.Background(), "tenant-1", "user-leaver", "", 2)
	if err != nil || len(items) != 2 || next != "act-2" {
		t.Fatalf("page = %d next = %q err = %v", len(items), next, err)
	}
	if other, _, err := service.Responsibilities(context.Background(), "tenant-1", "user-other", "", 10); err != nil || len(other) != 0 {
		t.Fatalf("other users' responsibilities leaked: %d (%v)", len(other), err)
	}
}

// 转移幂等；自转被拒。
func TestTransferIsIdempotentAndRejectsSelfSuccessor(t *testing.T) {
	repository := newRepositoryFake("act-1")
	service := serviceFor(t, repository)
	ctx := context.Background()

	input := TransferInput{TenantID: "tenant-1", BatchID: "batch-1", ItemID: "item-1", ResourceType: ResourceDunningAction,
		ResourceID: "act-1", FromUserID: "user-leaver", ToUserID: "user-successor", ExpectedVersion: 1,
		IdempotencyKey: "handover:batch-1:item-1"}
	receipt, err := service.Transfer(ctx, input)
	if err != nil || receipt.Status != ReceiptApplied || receipt.AppliedVersion != 2 {
		t.Fatalf("receipt = %+v err = %v", receipt, err)
	}
	if _, err := service.Transfer(ctx, input); err != nil {
		t.Fatalf("replay must succeed: %v", err)
	}
	if repository.transferCalls != 1 {
		t.Fatalf("transfer executed %d times, want exactly once", repository.transferCalls)
	}

	self := input
	self.IdempotencyKey, self.ToUserID = "handover:batch-1:item-2", input.FromUserID
	if _, err := service.Transfer(ctx, self); !errors.Is(err, ErrValidation) {
		t.Fatalf("self-successor must be rejected: %v", err)
	}
}

// 承接人处于交接冻结时不得接收新责任。
func TestTransferRejectsFrozenSuccessor(t *testing.T) {
	repository := newRepositoryFake("act-1")
	repository.freezes["tenant-1|batch-9|user-successor"] = Freeze{
		ID: "freeze-9", TenantID: "tenant-1", BatchID: "batch-9", UserID: "user-successor", Status: FreezeFrozen,
	}
	service := serviceFor(t, repository)

	_, err := service.Transfer(context.Background(), TransferInput{
		TenantID: "tenant-1", BatchID: "batch-1", ItemID: "item-1", ResourceType: ResourceDunningAction, ResourceID: "act-1",
		FromUserID: "user-leaver", ToUserID: "user-successor", ExpectedVersion: 1, IdempotencyKey: "handover:batch-1:item-1",
	})
	if !errors.Is(err, ErrFrozen) {
		t.Fatalf("err = %v, want ErrFrozen", err)
	}
	if repository.transferCalls != 0 {
		t.Fatal("transfer must not run when the successor is frozen")
	}
}

// 回执查询与完成解冻；非法结局被拒。
func TestReceiptLookupAndCompletion(t *testing.T) {
	repository := newRepositoryFake("act-1")
	service := serviceFor(t, repository)
	ctx := context.Background()

	if _, err := service.Receipt(ctx, "tenant-1", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown receipt must be reported: %v", err)
	}
	if _, err := service.Transfer(ctx, TransferInput{TenantID: "tenant-1", BatchID: "batch-1", ItemID: "item-1",
		ResourceType: ResourceDunningAction, ResourceID: "act-1", FromUserID: "user-leaver", ToUserID: "user-successor",
		ExpectedVersion: 1, IdempotencyKey: "handover:batch-1:item-1"}); err != nil {
		t.Fatal(err)
	}
	if receipt, err := service.Receipt(ctx, "tenant-1", "handover:batch-1:item-1"); err != nil || receipt.Status != ReceiptApplied {
		t.Fatalf("receipt = %+v (%v)", receipt, err)
	}
	if err := service.Complete(ctx, "tenant-1", "batch-1", "COMPLETED"); err != nil {
		t.Fatal(err)
	}
	if len(repository.released) != 1 || repository.released[0] != "tenant-1|batch-1|COMPLETED" {
		t.Fatalf("released = %v", repository.released)
	}
	if err := service.Complete(ctx, "tenant-1", "batch-1", "SOMETHING"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid outcome must be rejected: %v", err)
	}
}
