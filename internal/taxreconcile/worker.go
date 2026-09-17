package taxreconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/j-s-te/settlement/internal/service"
)

type Worker struct {
	DB          *sql.DB
	Service     *service.Service
	HTTP        *http.Client
	StatusURL   string
	AccessToken func(context.Context) (string, error)
	BatchSize   int
}

type candidate struct {
	table, id, tenant, externalRequestID string
	attempts                             int
}

// RunOnce claims due operations before making network calls, then feeds every
// result through the same transactional inbox/state machine as push callbacks.
func (w *Worker) RunOnce(ctx context.Context) error {
	if w.DB == nil || w.Service == nil || w.HTTP == nil || strings.TrimSpace(w.StatusURL) == "" || w.AccessToken == nil {
		return fmt.Errorf("tax reconciliation worker is not configured")
	}
	limit := w.BatchSize
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := w.DB.QueryContext(ctx, `(SELECT 'issue',id,tenant_id,external_request_id,reconcile_attempt_count FROM settlement_invoice_issue_attempt WHERE channel='TAX_ADAPTER' AND status IN ('ACCEPTED','UNKNOWN') AND next_reconcile_at<=UTC_TIMESTAMP(3)) UNION ALL (SELECT 'red',id,tenant_id,external_request_id,reconcile_attempt_count FROM settlement_invoice_red_flush_attempt WHERE status IN ('ACCEPTED','UNKNOWN') AND next_reconcile_at<=UTC_TIMESTAMP(3)) ORDER BY 5,2 LIMIT ?`, limit)
	if err != nil {
		return err
	}
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.table, &item.id, &item.tenant, &item.externalRequestID, &item.attempts); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range candidates {
		if err = w.reconcile(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) reconcile(ctx context.Context, item candidate) error {
	table := "settlement_invoice_issue_attempt"
	if item.table == "red" {
		table = "settlement_invoice_red_flush_attempt"
	}
	nextAttempt := time.Now().UTC().Add(NextDelay(item.attempts))
	result, err := w.DB.ExecContext(ctx, `UPDATE `+table+` SET last_reconciled_at=UTC_TIMESTAMP(3),next_reconcile_at=?,reconcile_attempt_count=reconcile_attempt_count+1 WHERE id=? AND tenant_id=? AND status IN ('ACCEPTED','UNKNOWN') AND next_reconcile_at<=UTC_TIMESTAMP(3)`, nextAttempt, item.id, item.tenant)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		return nil
	}
	token, err := w.AccessToken(ctx)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(w.StatusURL, "/") + "?external_request_id=" + url.QueryEscape(item.externalRequestID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return nil // unknown remains eligible for a later reconciliation attempt
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("tax status endpoint returned %s", resp.Status)
	}
	var event service.TaxCallbackEvent
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&event); err != nil {
		return fmt.Errorf("decode tax status result: %w", err)
	}
	if event.ExternalRequestID != item.externalRequestID || event.TenantID != item.tenant {
		return fmt.Errorf("tax status result identity mismatch")
	}
	_, err = w.Service.ApplyTaxCallback(ctx, item.tenant, event)
	return err
}

func NextDelay(attempt int) time.Duration {
	delays := []time.Duration{30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempt]
}
