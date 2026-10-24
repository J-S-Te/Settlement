package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/j-s-te/settlement/internal/handover"
	"github.com/j-s-te/settlement/internal/platform"
)

// handoverVerifier 只依赖"把令牌换成身份"这一能力，便于测试替换真实验签器。
type handoverVerifier interface {
	Verify(context.Context, string) (platform.ServiceIdentity, error)
}

// handoverBound 判断交接端点是否具备运行条件（服务 + 机器验签器）。
func (a *API) handoverBound() bool {
	return a != nil && a.handover != nil && a.handoverMachine != nil
}

// handoverTenant 只接受机器令牌绑定的租户；请求体或查询参数填写必须一致。
func (a *API) handoverTenant(w http.ResponseWriter, r *http.Request, requested string) (string, bool) {
	const bearerPrefix = "Bearer "
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(authorization, bearerPrefix) || strings.TrimSpace(strings.TrimPrefix(authorization, bearerPrefix)) == "" {
		fail(w, http.StatusUnauthorized, "SETTLEMENT_HANDOVER_BEARER_REQUIRED", "交接请求必须携带基础平台机器访问令牌")
		return "", false
	}
	identity, err := a.handoverMachine.Verify(r.Context(), strings.TrimSpace(strings.TrimPrefix(authorization, bearerPrefix)))
	if err != nil {
		fail(w, http.StatusUnauthorized, "SETTLEMENT_HANDOVER_BEARER_INVALID", "交接机器访问令牌无效")
		return "", false
	}
	tenant := strings.TrimSpace(identity.TenantID)
	if tenant == "" {
		fail(w, http.StatusUnauthorized, "SETTLEMENT_HANDOVER_BEARER_INVALID", "交接机器访问令牌缺少租户")
		return "", false
	}
	if strings.TrimSpace(requested) != "" && strings.TrimSpace(requested) != tenant {
		fail(w, http.StatusForbidden, "SETTLEMENT_HANDOVER_TENANT_MISMATCH", "请求租户与机器身份租户不一致")
		return "", false
	}
	return tenant, true
}

// handoverError 把交接错误映射为确定性状态码，平台据此判断是否重试。
func handoverError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, handover.ErrValidation):
		fail(w, http.StatusUnprocessableEntity, "SETTLEMENT_HANDOVER_VALIDATION", "交接请求参数不合法")
	case errors.Is(err, handover.ErrNotFound):
		fail(w, http.StatusNotFound, "SETTLEMENT_HANDOVER_NOT_FOUND", "交接记录不存在")
	case errors.Is(err, handover.ErrFrozen):
		fail(w, http.StatusConflict, "SETTLEMENT_HANDOVER_SUCCESSOR_FROZEN", "承接人处于交接冻结状态")
	case errors.Is(err, handover.ErrConflict):
		fail(w, http.StatusConflict, "SETTLEMENT_HANDOVER_CONFLICT", "交接状态冲突")
	default:
		fail(w, http.StatusInternalServerError, "SETTLEMENT_HANDOVER_INTERNAL", "交接处理失败")
	}
}

func decodeHandover(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		fail(w, http.StatusUnprocessableEntity, "SETTLEMENT_HANDOVER_VALIDATION", "交接请求参数不合法")
		return false
	}
	return true
}

type handoverFreezePayload struct {
	TenantID  string `json:"tenant_id"`
	BatchID   string `json:"batch_id"`
	RequestID string `json:"request_id"`
	UserID    string `json:"user_id"`
}

type handoverTransferPayload struct {
	TenantID        string `json:"tenant_id"`
	BatchID         string `json:"batch_id"`
	ItemID          string `json:"item_id"`
	RequestID       string `json:"request_id"`
	ResourceType    string `json:"resource_type"`
	ResourceID      string `json:"resource_id"`
	FromUserID      string `json:"from_user_id"`
	ToUserID        string `json:"to_user_id"`
	ExpectedVersion uint64 `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
}

type handoverCompletePayload struct {
	TenantID string `json:"tenant_id"`
	BatchID  string `json:"batch_id"`
	Outcome  string `json:"outcome"`
}

// handoverFreeze 冻结责任并返回权威责任总数。
func (a *API) handoverFreeze(w http.ResponseWriter, r *http.Request) {
	var payload handoverFreezePayload
	if !decodeHandover(w, r, &payload) {
		return
	}
	tenant, ok := a.handoverTenant(w, r, payload.TenantID)
	if !ok {
		return
	}
	freeze, count, err := a.handover.Freeze(r.Context(), tenant, payload.BatchID, payload.RequestID, payload.UserID)
	if err != nil {
		handoverError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"system": "settlement", "frozen": freeze.Status == handover.FreezeFrozen,
		"frozen_at": freeze.FrozenAt, "responsibility_count": count})
}

// handoverResponsibilities 返回分页权威责任清单。
func (a *API) handoverResponsibilities(w http.ResponseWriter, r *http.Request) {
	tenant, ok := a.handoverTenant(w, r, r.URL.Query().Get("tenant_id"))
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, next, err := a.handover.Responsibilities(r.Context(), tenant, r.URL.Query().Get("user_id"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		handoverError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"system": "settlement", "items": items, "next_cursor": next})
}

// handoverTransfer 逐项转移并返回幂等回执。
func (a *API) handoverTransfer(w http.ResponseWriter, r *http.Request) {
	var payload handoverTransferPayload
	if !decodeHandover(w, r, &payload) {
		return
	}
	tenant, ok := a.handoverTenant(w, r, payload.TenantID)
	if !ok {
		return
	}
	receipt, err := a.handover.Transfer(r.Context(), handover.TransferInput{
		TenantID: tenant, BatchID: payload.BatchID, ItemID: payload.ItemID, RequestID: payload.RequestID,
		ResourceType: payload.ResourceType, ResourceID: payload.ResourceID,
		FromUserID: payload.FromUserID, ToUserID: payload.ToUserID,
		ExpectedVersion: payload.ExpectedVersion, IdempotencyKey: payload.IdempotencyKey,
	})
	if err != nil {
		handoverError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"system": "settlement", "item_id": receipt.ItemID,
		"idempotency_key": receipt.IdempotencyKey, "status": receipt.Status, "applied_version": receipt.AppliedVersion,
		"reason_code": receipt.ReasonCode, "evidence_ref": receipt.EvidenceRef, "received_at": receipt.ReceivedAt})
}

// handoverReceipt 查询既有回执（幂等核对入口）。
func (a *API) handoverReceipt(w http.ResponseWriter, r *http.Request) {
	tenant, ok := a.handoverTenant(w, r, r.URL.Query().Get("tenant_id"))
	if !ok {
		return
	}
	receipt, err := a.handover.Receipt(r.Context(), tenant, r.URL.Query().Get("idempotency_key"))
	if err != nil {
		handoverError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"system": "settlement", "item_id": receipt.ItemID,
		"idempotency_key": receipt.IdempotencyKey, "status": receipt.Status, "applied_version": receipt.AppliedVersion,
		"reason_code": receipt.ReasonCode, "evidence_ref": receipt.EvidenceRef, "received_at": receipt.ReceivedAt})
}

// handoverComplete 结束交接：解除冻结，已完成的转移保留。
func (a *API) handoverComplete(w http.ResponseWriter, r *http.Request) {
	var payload handoverCompletePayload
	if !decodeHandover(w, r, &payload) {
		return
	}
	tenant, ok := a.handoverTenant(w, r, payload.TenantID)
	if !ok {
		return
	}
	if err := a.handover.Complete(r.Context(), tenant, payload.BatchID, payload.Outcome); err != nil {
		handoverError(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"system": "settlement", "batch_id": payload.BatchID,
		"outcome": strings.ToUpper(strings.TrimSpace(payload.Outcome))})
}
