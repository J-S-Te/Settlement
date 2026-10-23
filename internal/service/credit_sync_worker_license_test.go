package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/j-s-te/settlement/internal/creditclient"
	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

type licenseCreditPublisher struct {
	send func(string) error
}

func TestCreditLicensePauseInsidePublisherKeepsClaimRecoverable(t *testing.T) {
	var expired atomic.Bool
	var posts atomic.Int32
	denied := errors.New("expired during token acquisition")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			expired.Store(true)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"credit-test-token","token_type":"Bearer","expires_in":300}`)
			return
		}
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	publisher, err := creditclient.New(server.URL+"/events", server.URL+"/token", "client", "secret", "credit.write", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	check := func(context.Context) error {
		if expired.Load() {
			return denied
		}
		return nil
	}
	publisher.SetBusinessLicenseCheck(check)
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FOR UPDATE SKIP LOCKED", Rows: [][]driver.Value{{"row-1", "tenant", "event", []byte(`{}`)}}},
		scriptdb.Step{Kind: "exec", SQL: "status='PROCESSING'"},
		scriptdb.Step{Kind: "commit"},
	)
	w := &CreditSyncWorker{DB: db, Publisher: publisher, CheckBusinessLicense: check}
	if err := w.RunOnce(context.Background()); !errors.Is(err, denied) || posts.Load() != 0 {
		t.Fatalf("paused transport must leave lease without failure/outcome writes: posts=%d error=%v", posts.Load(), err)
	}
}

func (p licenseCreditPublisher) Publish(_ context.Context, _, eventID string, _ []byte) error {
	return p.send(eventID)
}

func TestCreditLicenseExpiryAfterClaimPreventsPublish(t *testing.T) {
	denied := errors.New("license expired after claim")
	expired, published := false, 0
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FOR UPDATE SKIP LOCKED", Rows: [][]driver.Value{{"row-1", "tenant", "event-1", []byte(`{}`)}}},
		scriptdb.Step{Kind: "exec", SQL: "status='PROCESSING'"},
		scriptdb.Step{Kind: "commit", After: func() { expired = true }},
	)
	w := &CreditSyncWorker{DB: db, Publisher: licenseCreditPublisher{send: func(string) error { published++; return nil }}, CheckBusinessLicense: func(context.Context) error {
		if expired {
			return denied
		}
		return nil
	}}
	if err := w.RunOnce(context.Background()); !errors.Is(err, denied) || published != 0 {
		t.Fatalf("expired claimed event published=%d error=%v", published, err)
	}
	// 禁止任何结果 UPDATE：已提交的 PROCESSING 租约
	// 在截止后仍可被现有过期租约查询重新认领。
}

func TestCreditLicenseExpiryBetweenEventsPreservesFirstOutcome(t *testing.T) {
	denied := errors.New("license expired after first delivery")
	expired := false
	var published []string
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FOR UPDATE SKIP LOCKED", Rows: [][]driver.Value{{"row-1", "tenant", "event-1", []byte(`{}`)}, {"row-2", "tenant", "event-2", []byte(`{}`)}}},
		scriptdb.Step{Kind: "exec", SQL: "status='PROCESSING'"},
		scriptdb.Step{Kind: "exec", SQL: "status='PROCESSING'"},
		scriptdb.Step{Kind: "commit"},
		scriptdb.Step{Kind: "exec", SQL: "status='DELIVERED'"},
	)
	w := &CreditSyncWorker{DB: db, Publisher: licenseCreditPublisher{send: func(id string) error { published = append(published, id); expired = true; return nil }}, CheckBusinessLicense: func(context.Context) error {
		if expired {
			return denied
		}
		return nil
	}}
	if err := w.RunOnce(context.Background()); !errors.Is(err, denied) || len(published) != 1 || published[0] != "event-1" {
		t.Fatalf("batch continued after expiry: events=%v error=%v", published, err)
	}
}

func TestCreditLicenseExpiryDuringClaimRollsBackWithoutAttempt(t *testing.T) {
	denied := errors.New("license expired during lock wait")
	expired := false
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FOR UPDATE SKIP LOCKED", Rows: [][]driver.Value{{"row-1", "tenant", "event-1", []byte(`{}`)}}, After: func() { expired = true }},
		scriptdb.Step{Kind: "rollback"},
	)
	w := &CreditSyncWorker{DB: db, Publisher: licenseCreditPublisher{send: func(string) error { t.Error("business publish after expiry"); return nil }}, CheckBusinessLicense: func(context.Context) error {
		if expired {
			return denied
		}
		return nil
	}}
	if err := w.RunOnce(context.Background()); !errors.Is(err, denied) {
		t.Fatalf("claim should roll back on expired license: %v", err)
	}
}

func TestCreditPublishFailureRecordingErrorIsReturned(t *testing.T) {
	storageErr := errors.New("cannot record delivery failure")
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FOR UPDATE SKIP LOCKED", Rows: [][]driver.Value{{"row-1", "tenant", "event-1", []byte(`{}`)}}},
		scriptdb.Step{Kind: "exec", SQL: "status='PROCESSING'"},
		scriptdb.Step{Kind: "commit"},
		scriptdb.Step{Kind: "exec", SQL: "status='RETRY_WAIT'", Err: storageErr},
	)
	w := &CreditSyncWorker{DB: db, Publisher: licenseCreditPublisher{send: func(string) error { return errors.New("CRM unavailable") }}, CheckBusinessLicense: func(context.Context) error { return nil }}
	if err := w.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), storageErr.Error()) {
		t.Fatalf("failure recording error was lost: %v", err)
	}
}

func TestCreditLicenseExpiryBeforeClaimCommitRollsBackAttempts(t *testing.T) {
	denied := errors.New("expired after claim update")
	expired := false
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FOR UPDATE SKIP LOCKED", Rows: [][]driver.Value{{"row-1", "tenant", "event-1", []byte(`{}`)}}},
		scriptdb.Step{Kind: "exec", SQL: "attempt_count=attempt_count+1", After: func() { expired = true }},
		scriptdb.Step{Kind: "rollback"},
	)
	w := &CreditSyncWorker{DB: db, Publisher: licenseCreditPublisher{send: func(string) error { t.Error("publish after expiry"); return nil }}, CheckBusinessLicense: func(context.Context) error {
		if expired {
			return denied
		}
		return nil
	}}
	if err := w.RunOnce(context.Background()); !errors.Is(err, denied) {
		t.Fatalf("expired claim committed: %v", err)
	}
}
