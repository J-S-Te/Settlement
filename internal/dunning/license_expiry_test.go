package dunning

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/settlement/internal/testutil/scriptdb"
)

type expiringDirectory struct{ expire func() }

func (d expiringDirectory) ContainsCollector(context.Context, string) (bool, error) {
	d.expire()
	return true, nil
}

func TestDunningLicenseExpiryAfterDirectoryPreventsInitialCreation(t *testing.T) {
	denied := errors.New("expired during directory request")
	expired := false
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_dunning_action"},
		scriptdb.Step{Kind: "exec", SQL: "SET dc.status='CLOSED'"},
		scriptdb.Step{Kind: "exec", SQL: "SET dc.status='ACTIVE'"},
		scriptdb.Step{Kind: "query", SQL: "WHERE r.open_amount>0", Rows: [][]driver.Value{{"tenant", "receivable", "R-1", "customer", "policy", int64(1), "REMIND", "USER:collector", "LOCAL", "NORMAL", int64(7), int64(5)}}},
		scriptdb.Step{Kind: "query", SQL: "settlement_handover_freeze", Rows: [][]driver.Value{{false}}},
	)
	s := &Scanner{DB: db, Directory: expiringDirectory{expire: func() { expired = true }}, CheckBusinessLicense: func(context.Context) error {
		if expired {
			return denied
		}
		return nil
	}}
	if err := s.RunOnce(context.Background()); !errors.Is(err, denied) {
		t.Fatalf("initial responsibility must stop after delayed expiry: %v", err)
	}
}

func TestDunningLicenseExpiryAfterCaseLockPreventsRepeat(t *testing.T) {
	denied := errors.New("expired during case lock")
	expired := false
	db := scriptdb.Open(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "query", SQL: "FOR UPDATE", Rows: [][]driver.Value{{"ACTIVE", time.Now().Add(-time.Hour)}}, After: func() { expired = true }},
		scriptdb.Step{Kind: "rollback"},
	)
	s := &Scanner{DB: db, CheckBusinessLicense: func(context.Context) error {
		if expired {
			return denied
		}
		return nil
	}}
	if err := s.createRepeat(context.Background(), dueCase{TenantID: "tenant", CaseID: "case"}, "collector"); !errors.Is(err, denied) {
		t.Fatalf("repeat action must stop after lock wait: %v", err)
	}
}

func TestDunningLicenseExpiryDuringBeginPreventsInitialWrite(t *testing.T) {
	denied := errors.New("expired during transaction begin")
	expired := false
	db := scriptdb.Open(t, scriptdb.Step{Kind: "begin", After: func() { expired = true }}, scriptdb.Step{Kind: "rollback"})
	s := &Scanner{DB: db, CheckBusinessLicense: func(context.Context) error {
		if expired {
			return denied
		}
		return nil
	}}
	if err := s.createInitial(context.Background(), candidate{}, "collector"); !errors.Is(err, denied) {
		t.Fatalf("initial write must stop after begin wait: %v", err)
	}
}

func TestDunningLicenseExpiryBetweenClosureWritesStopsNextWrite(t *testing.T) {
	denied := errors.New("expired after action closure")
	expired := false
	db := scriptdb.Open(t, scriptdb.Step{Kind: "exec", SQL: "UPDATE settlement_dunning_action", After: func() { expired = true }})
	s := &Scanner{DB: db, CheckBusinessLicense: func(context.Context) error {
		if expired {
			return denied
		}
		return nil
	}}
	if err := s.closeSettled(context.Background()); !errors.Is(err, denied) {
		t.Fatalf("case closure must stop after expiry: %v", err)
	}
}

func TestDunningExpiredStateChangesDoNotWrite(t *testing.T) {
	for _, name := range []string{"reopen", "defer"} {
		t.Run(name, func(t *testing.T) {
			denied := errors.New("expired")
			s := &Scanner{DB: scriptdb.Open(t), CheckBusinessLicense: func(context.Context) error { return denied }}
			var err error
			if name == "reopen" {
				err = s.reopenSettled(context.Background())
			} else {
				err = s.deferCase(context.Background(), dueCase{})
			}
			if !errors.Is(err, denied) {
				t.Fatalf("state changed after expiry: %v", err)
			}
		})
	}
}

func TestDunningMidTransactionExpiryRollsBackAllNewWork(t *testing.T) {
	for _, stop := range []string{"case", "action", "notification", "outbox"} {
		t.Run(stop, func(t *testing.T) {
			denied := errors.New("expired during transaction")
			expired := false
			steps := []scriptdb.Step{{Kind: "begin"}}
			for _, write := range []struct{ name, sql string }{
				{"case", "INSERT IGNORE INTO settlement_dunning_case"},
				{"action", "INSERT INTO settlement_dunning_action"},
				{"notification", "INSERT INTO settlement_local_notification"},
				{"outbox", "INSERT INTO settlement_outbox_event"},
			} {
				step := scriptdb.Step{Kind: "exec", SQL: write.sql}
				if write.name == stop {
					step.After = func() { expired = true }
					steps = append(steps, step)
					break
				}
				steps = append(steps, step)
			}
			steps = append(steps, scriptdb.Step{Kind: "rollback"})
			s := &Scanner{DB: scriptdb.Open(t, steps...), CheckBusinessLicense: func(context.Context) error {
				if expired {
					return denied
				}
				return nil
			}}
			if err := s.createInitial(context.Background(), candidate{Channel: "PLATFORM"}, "collector"); !errors.Is(err, denied) {
				t.Fatalf("partial new work must roll back at %s: %v", stop, err)
			}
		})
	}
}

func TestDunningLicensedCreationStillCommits(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "repeat"}[repeat], func(t *testing.T) {
			steps := []scriptdb.Step{{Kind: "begin"}}
			if repeat {
				steps = append(steps, scriptdb.Step{Kind: "query", SQL: "FOR UPDATE", Rows: [][]driver.Value{{"ACTIVE", time.Now().Add(-time.Hour)}}})
			} else {
				steps = append(steps, scriptdb.Step{Kind: "exec", SQL: "INSERT IGNORE INTO settlement_dunning_case"})
			}
			steps = append(steps, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_dunning_action"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_local_notification"}, scriptdb.Step{Kind: "exec", SQL: "INSERT INTO settlement_outbox_event"})
			if repeat {
				steps = append(steps, scriptdb.Step{Kind: "exec", SQL: "SET current_escalation_level="})
			}
			steps = append(steps, scriptdb.Step{Kind: "commit"})
			s := &Scanner{DB: scriptdb.Open(t, steps...), CheckBusinessLicense: func(context.Context) error { return nil }}
			var err error
			if repeat {
				err = s.createRepeat(context.Background(), dueCase{Channel: "PLATFORM"}, "collector")
			} else {
				err = s.createInitial(context.Background(), candidate{Channel: "PLATFORM"}, "collector")
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
