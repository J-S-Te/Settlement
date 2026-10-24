package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/handover"
	"github.com/j-s-te/settlement/internal/platform"
)

type handoverVerifierStub struct {
	identity platform.ServiceIdentity
	err      error
}

func (stub handoverVerifierStub) Verify(context.Context, string) (platform.ServiceIdentity, error) {
	if stub.err != nil {
		return platform.ServiceIdentity{}, stub.err
	}
	return stub.identity, nil
}

type handoverRepositoryStub struct {
	freezeTenant string
	freezeUser   string
	responsibles []handover.Responsibility
	receipt      handover.Receipt
	receiptFound bool
}

func (stub *handoverRepositoryStub) CreateFreeze(_ context.Context, freeze handover.Freeze) (handover.Freeze, bool, error) {
	stub.freezeTenant, stub.freezeUser = freeze.TenantID, freeze.UserID
	return freeze, true, nil
}

func (stub *handoverRepositoryStub) ReleaseFreeze(context.Context, string, string, string, time.Time) error {
	return nil
}

func (stub *handoverRepositoryStub) ActiveFreeze(context.Context, string, string) (handover.Freeze, bool, error) {
	return handover.Freeze{}, false, nil
}

func (stub *handoverRepositoryStub) ListResponsibilities(context.Context, string, string, string, int) ([]handover.Responsibility, error) {
	return stub.responsibles, nil
}

func (stub *handoverRepositoryStub) TransferOwnership(context.Context, handover.TransferInput, time.Time) (handover.Receipt, error) {
	return handover.Receipt{Status: handover.ReceiptApplied, AppliedVersion: 1, EvidenceRef: "settlement:dunning_action:act-1"}, nil
}

func (stub *handoverRepositoryStub) FindReceipt(context.Context, string, string) (handover.Receipt, bool, error) {
	return stub.receipt, stub.receiptFound, nil
}

func handoverAPI(t *testing.T, repository *handoverRepositoryStub, verifier handoverVerifier) *API {
	t.Helper()
	service, err := handover.NewService(repository, func(time.Time) (string, error) {
		return "01STHANDOVER0000000000000000001", nil
	}, func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return &API{handover: service, handoverMachine: verifier, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func handoverCall(t *testing.T, api *API, handler http.HandlerFunc, method, target, body, bearer string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response := httptest.NewRecorder()
	handler(response, request)
	payload := map[string]any{}
	_ = json.Unmarshal(response.Body.Bytes(), &payload)
	return response.Code, payload
}

// 交接端点必须要求机器令牌；令牌无效不得触达服务。
func TestSettlementHandoverRequiresBearer(t *testing.T) {
	repository := &handoverRepositoryStub{}
	api := handoverAPI(t, repository, handoverVerifierStub{identity: platform.ServiceIdentity{TenantID: "tenant-1"}})

	status, payload := handoverCall(t, api, api.handoverFreeze, http.MethodPost, "/internal/v1/handover/freeze", `{}`, "")
	if status != http.StatusUnauthorized || payload["code"] != "SETTLEMENT_HANDOVER_BEARER_REQUIRED" {
		t.Fatalf("missing bearer → %d %v", status, payload)
	}
	invalid := handoverAPI(t, repository, handoverVerifierStub{err: errors.New("bad token")})
	status, payload = handoverCall(t, invalid, invalid.handoverFreeze, http.MethodPost, "/internal/v1/handover/freeze", `{}`, "token")
	if status != http.StatusUnauthorized || payload["code"] != "SETTLEMENT_HANDOVER_BEARER_INVALID" {
		t.Fatalf("invalid bearer → %d %v", status, payload)
	}
	if repository.freezeUser != "" {
		t.Fatal("unauthenticated request must not reach the service")
	}
}

// 租户只取令牌绑定；请求体覆盖必须 403。
func TestSettlementHandoverBindsTenantFromToken(t *testing.T) {
	repository := &handoverRepositoryStub{}
	api := handoverAPI(t, repository, handoverVerifierStub{identity: platform.ServiceIdentity{TenantID: "tenant-1"}})

	status, payload := handoverCall(t, api, api.handoverFreeze, http.MethodPost, "/internal/v1/handover/freeze",
		`{"tenant_id":"tenant-attacker","batch_id":"b1","request_id":"r1","user_id":"u1"}`, "machine-token")
	if status != http.StatusForbidden || payload["code"] != "SETTLEMENT_HANDOVER_TENANT_MISMATCH" {
		t.Fatalf("tenant override → %d %v", status, payload)
	}

	status, payload = handoverCall(t, api, api.handoverFreeze, http.MethodPost, "/internal/v1/handover/freeze",
		`{"batch_id":"b1","request_id":"r1","user_id":"u1"}`, "machine-token")
	if status != http.StatusOK {
		t.Fatalf("freeze → %d %v", status, payload)
	}
	if repository.freezeTenant != "tenant-1" || repository.freezeUser != "u1" {
		t.Fatalf("service saw tenant=%q user=%q", repository.freezeTenant, repository.freezeUser)
	}
	data, ok := payload["data"].(map[string]any)
	if !ok || data["system"] != "settlement" || data["frozen"] != true {
		t.Fatalf("freeze payload = %v", payload)
	}
}

// 参数不完整 → 422；正常转移 → 200 且回执字段齐全。
func TestSettlementHandoverTransferValidationAndReceipt(t *testing.T) {
	repository := &handoverRepositoryStub{}
	api := handoverAPI(t, repository, handoverVerifierStub{identity: platform.ServiceIdentity{TenantID: "tenant-1"}})

	status, payload := handoverCall(t, api, api.handoverTransfer, http.MethodPost, "/internal/v1/handover/transfers", `{"item_id":"i1"}`, "machine-token")
	if status != http.StatusUnprocessableEntity || payload["code"] != "SETTLEMENT_HANDOVER_VALIDATION" {
		t.Fatalf("incomplete transfer → %d %v", status, payload)
	}

	status, payload = handoverCall(t, api, api.handoverTransfer, http.MethodPost, "/internal/v1/handover/transfers",
		`{"batch_id":"b1","item_id":"i1","resource_type":"dunning_action","resource_id":"act-1","from_user_id":"u1","to_user_id":"u2","expected_version":1,"idempotency_key":"handover:b1:i1"}`, "machine-token")
	if status != http.StatusOK {
		t.Fatalf("transfer → %d %v", status, payload)
	}
	data, ok := payload["data"].(map[string]any)
	if !ok || data["status"] != handover.ReceiptApplied || data["evidence_ref"] == "" {
		t.Fatalf("transfer payload = %v", payload)
	}
}
