package licensecore

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// TimeStore must preserve a monotonic high-water mark atomically across callers.
// Implementations must not overwrite a larger timestamp with a smaller one.
type TimeStore interface {
	Load(context.Context) (int64, error)
	SaveMax(context.Context, int64) error
}

// TimeObserver is an optional rollback detector. Deployments must supply durable
// storage and call Observe before evaluating operations; this package does not
// claim to prevent OS clock tampering or independently integrate any service.
type TimeObserver struct {
	Store     TimeStore
	Tolerance time.Duration
	mu        sync.Mutex
}

func (o *TimeObserver) Observe(ctx context.Context, now time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Store == nil || o.Tolerance < 0 || !validTime(now.Unix()) {
		return ErrInvalid
	}
	previous, err := o.Store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load license time observation: %w", err)
	}
	if previous < 0 || previous > maxTimestamp {
		return ErrInvalid
	}
	if previous > now.Unix() && previous-now.Unix() > int64(o.Tolerance/time.Second) {
		return ErrRollback
	}
	if err = o.Store.SaveMax(ctx, now.Unix()); err != nil {
		return fmt.Errorf("save license time observation: %w", err)
	}
	return nil
}
