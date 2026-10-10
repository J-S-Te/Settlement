package outbox

import (
	"context"
	"errors"
	"testing"
)

func TestExpiredTaxDeliveryDoesNotAcquireOrConsumeRetry(t *testing.T) {
	w := &Worker{Destinations: []Destination{{Name: "TAX_INVOICE_COMMAND"}}, CheckBusinessLicense: func(context.Context) error { return errors.New("expired") }}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.deliver(context.Background(), Destination{Name: "TAX_INVOICE_COMMAND"}, nil); !errors.Is(err, ErrLicensePaused) {
		t.Fatal(err)
	}
}
