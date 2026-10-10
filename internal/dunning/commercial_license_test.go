package dunning

import (
	"context"
	"errors"
	"testing"
)

func TestExpiredDunningStopsBeforeDatabase(t *testing.T) {
	denied := errors.New("expired")
	s := &Scanner{CheckBusinessLicense: func(context.Context) error { return denied }}
	if !errors.Is(s.RunOnce(context.Background()), denied) {
		t.Fatal("business scan continued")
	}
}
