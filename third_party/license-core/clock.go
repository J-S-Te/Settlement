package licensecore

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// TimeStore 须在并发调用间原子地维护单调递增的时间高水位。
// 实现不得用较小时间戳覆盖较大时间戳。
type TimeStore interface {
	Load(context.Context) (int64, error)
	SaveMax(context.Context, int64) error
}

// TimeObserver 提供可选的时钟回拨检测。部署时须提供持久化存储，
// 并在评估操作前调用 Observe；本包不负责防止操作系统时钟被篡改，
// 也不会自行接入任何服务。
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
