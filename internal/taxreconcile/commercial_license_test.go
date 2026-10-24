package taxreconcile

import (
	"context"
	"errors"
	"testing"
)

func TestExpiredReconciliationStopsBeforeDatabaseOrToken(t *testing.T) {
	denied := errors.New("expired")
	w := &Worker{CheckBusinessLicense: func(context.Context) error { return denied }}
	if !errors.Is(w.RunOnce(context.Background()), denied) {
		t.Fatal("business reconciliation continued")
	}
}
