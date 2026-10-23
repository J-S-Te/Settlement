package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/settlement/internal/creditclient"
)

// CreditSyncWorker 重试未通过请求路径投递的 CRM 信用
// 事件。认领通过 locked_at 取得租约，崩溃进程可在
// 租约过期后被重新接管，同时防止第二个工作器并发发送
// 相同事件。
type CreditSyncWorker struct {
	DB                   *sql.DB
	Publisher            CreditEventPublisher
	BatchSize            int
	Lease                time.Duration
	Now                  func() time.Time
	CheckBusinessLicense func(context.Context) error
}

type claimedCreditEvent struct {
	id       string
	tenantID string
	eventID  string
	payload  []byte
}

func (w *CreditSyncWorker) RunOnce(ctx context.Context) error {
	if w == nil || w.DB == nil {
		return errors.New("credit sync worker database is not configured")
	}
	if w.Publisher == nil {
		return errors.New("credit sync worker publisher is not configured")
	}
	if w.CheckBusinessLicense != nil {
		if err := w.CheckBusinessLicense(ctx); err != nil {
			return err
		}
	}
	batch := w.BatchSize
	if batch <= 0 {
		batch = 50
	}
	lease := w.Lease
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	now := time.Now().UTC()
	if w.Now != nil {
		now = w.Now().UTC()
	}
	events, err := w.claim(ctx, now, lease, batch)
	if err != nil {
		return err
	}
	for _, event := range events {
		// 锁等待或前次投递可能跨越到期时间。未发送的
		// 认领行保留租约，以便正常过期租约恢复时重试；
		// 许可证暂停不属于 CRM 投递失败。
		if err := w.checkBusinessLicense(ctx); err != nil {
			return err
		}
		if err := w.Publisher.Publish(ctx, event.tenantID, event.eventID, event.payload); err != nil {
			if errors.Is(err, creditclient.ErrLicensePaused) {
				return err
			}
			_, updateErr := w.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE settlement_credit_sync SET status='RETRY_WAIT',locked_at=NULL,last_error_summary=?,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND status='PROCESSING'`, safeCreditError(err), event.id)
			if updateErr != nil {
				return fmt.Errorf("publish CRM credit event: %w; record failure: %v", err, updateErr)
			}
			continue
		}
		if _, err := w.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE settlement_credit_sync SET status='DELIVERED',delivered_at=UTC_TIMESTAMP(3),locked_at=NULL,last_error_summary='',updated_at=UTC_TIMESTAMP(3) WHERE id=? AND status='PROCESSING'`, event.id); err != nil {
			return err
		}
	}
	return nil
}

func (w *CreditSyncWorker) claim(ctx context.Context, now time.Time, lease time.Duration, batch int) ([]claimedCreditEvent, error) {
	if err := w.checkBusinessLicense(ctx); err != nil {
		return nil, err
	}
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,tenant_id,event_id,payload_json FROM settlement_credit_sync WHERE status IN ('PENDING','RETRY_WAIT') OR (status='PROCESSING' AND locked_at IS NOT NULL AND locked_at<?) ORDER BY updated_at,id LIMIT ? FOR UPDATE SKIP LOCKED`, now.Add(-lease), batch)
	if err != nil {
		return nil, err
	}
	events := make([]claimedCreditEvent, 0)
	for rows.Next() {
		var event claimedCreditEvent
		if err = rows.Scan(&event.id, &event.tenantID, &event.eventID, &event.payload); err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, event)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, event := range events {
		if err := w.checkBusinessLicense(ctx); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settlement_credit_sync SET status='PROCESSING',locked_at=?,last_attempt_at=?,attempt_count=attempt_count+1,updated_at=? WHERE id=?`, now, now, now, event.id); err != nil {
			return nil, err
		}
	}
	if err := w.checkBusinessLicense(ctx); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

func (w *CreditSyncWorker) checkBusinessLicense(ctx context.Context) error {
	if w.CheckBusinessLicense != nil {
		return w.CheckBusinessLicense(ctx)
	}
	return nil
}
