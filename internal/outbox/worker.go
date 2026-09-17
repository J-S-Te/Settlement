package outbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrLeaseLost = errors.New("outbox lease lost")

type Event struct {
	ID, TenantID, EventID, Destination, EventType, AggregateType, AggregateID string
	Payload                                                                   json.RawMessage
	Attempt                                                                   int
	CreatedAt                                                                 time.Time
}
type Store struct {
	DB       *sql.DB
	WorkerID string
	Lease    time.Duration
}

func (s *Store) Acquire(ctx context.Context, destination string, limit int) ([]Event, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,tenant_id,event_id,destination,event_type,aggregate_type,aggregate_id,payload_json,attempt_count,created_at FROM settlement_outbox_event WHERE destination=? AND ((status IN('PENDING','RETRY_WAIT') AND available_at<=UTC_TIMESTAMP(3)) OR (status='PROCESSING' AND locked_until<UTC_TIMESTAMP(3))) ORDER BY created_at,id LIMIT ? FOR UPDATE SKIP LOCKED`, destination, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TenantID, &e.EventID, &e.Destination, &e.EventType, &e.AggregateType, &e.AggregateID, &e.Payload, &e.Attempt, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	until := time.Now().UTC().Add(s.Lease)
	for _, e := range events {
		result, err := tx.ExecContext(ctx, `UPDATE settlement_outbox_event SET status='PROCESSING',locked_by=?,locked_until=?,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND (locked_by IS NULL OR locked_until<UTC_TIMESTAMP(3))`, s.WorkerID, until, e.ID)
		if err != nil {
			return nil, err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return nil, ErrLeaseLost
		}
	}
	return events, tx.Commit()
}
func (s *Store) Delivered(ctx context.Context, events []Event) error {
	for _, e := range events {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE settlement_outbox_event SET status='DELIVERED',delivered_at=UTC_TIMESTAMP(3),locked_by=NULL,locked_until=NULL,last_error_code='',last_error_summary='',updated_at=UTC_TIMESTAMP(3) WHERE id=? AND status='PROCESSING' AND locked_by=?`, e.ID, s.WorkerID)
		if err != nil {
			tx.Rollback()
			return err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			tx.Rollback()
			return ErrLeaseLost
		}
		if e.Destination == "TAX_INVOICE_COMMAND" {
			var payload struct {
				AttemptID         string `json:"attempt_id"`
				RedFlushAttemptID string `json:"red_flush_attempt_id"`
			}
			if err = json.Unmarshal(e.Payload, &payload); err != nil {
				tx.Rollback()
				return errors.New("invalid tax command payload")
			}
			if payload.AttemptID != "" {
				_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET status='ACCEPTED',accepted_at=COALESCE(accepted_at,UTC_TIMESTAMP(3)),next_reconcile_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 1 MINUTE) WHERE id=? AND tenant_id=? AND status='PENDING'`, payload.AttemptID, e.TenantID)
			} else if payload.RedFlushAttemptID != "" {
				_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_attempt SET status='ACCEPTED',accepted_at=COALESCE(accepted_at,UTC_TIMESTAMP(3)),next_reconcile_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 1 MINUTE) WHERE id=? AND tenant_id=? AND status='PENDING'`, payload.RedFlushAttemptID, e.TenantID)
			} else {
				err = errors.New("tax command is missing attempt id")
			}
			if err != nil {
				tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) Failed(ctx context.Context, events []Event, code, summary string, permanent bool) error {
	for _, e := range events {
		attempt := e.Attempt + 1
		dead := permanent || attempt >= 7
		status := "RETRY_WAIT"
		available := time.Now().UTC().Add(backoff(attempt))
		var deadAt any = nil
		if dead {
			status = "DEAD_LETTER"
			deadAt = time.Now().UTC()
			available = time.Now().UTC()
		}
		result, err := s.DB.ExecContext(ctx, `UPDATE settlement_outbox_event SET status=?,attempt_count=?,available_at=?,locked_by=NULL,locked_until=NULL,last_error_code=?,last_error_summary=?,dead_lettered_at=?,updated_at=UTC_TIMESTAMP(3) WHERE id=? AND status='PROCESSING' AND locked_by=?`, status, attempt, available, safe(code, 128), safe(summary, 500), deadAt, e.ID, s.WorkerID)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return ErrLeaseLost
		}
	}
	return nil
}
func backoff(attempt int) time.Duration {
	values := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour}
	if attempt < 1 {
		return values[0]
	}
	if attempt > len(values) {
		return values[len(values)-1]
	}
	return values[attempt-1]
}
func safe(value string, max int) string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\r", " ")
	if len(value) > max {
		return value[:max]
	}
	return value
}

type Destination struct {
	Name, Endpoint, Scope, ClientID, ClientSecret string
	TokenEndpoint                                 string
	ApplicationCode, EnvironmentCode              string
}
type Worker struct {
	Store         *Store
	HTTP          *http.Client
	TokenEndpoint string
	BatchSize     int
	Destinations  []Destination
	mu            sync.Mutex
	tokens        map[string]cachedToken
}
type cachedToken struct {
	Value   string
	Expires time.Time
}

func (w *Worker) Run(ctx context.Context) error {
	if w.HTTP == nil {
		w.HTTP = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if w.BatchSize < 1 || w.BatchSize > 100 {
		w.BatchSize = 50
	}
	w.tokens = map[string]cachedToken{}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (w *Worker) RunOnce(ctx context.Context) error {
	for _, destination := range w.Destinations {
		events, err := w.Store.Acquire(ctx, destination.Name, w.BatchSize)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			continue
		}
		if err = w.deliver(ctx, destination, events); err != nil {
			permanent := false
			var status *httpStatusError
			if errors.As(err, &status) {
				permanent = status.Status == 400 || status.Status == 413 || status.Status == 422
			}
			_ = w.Store.Failed(context.WithoutCancel(ctx), events, "DELIVERY_FAILED", err.Error(), permanent)
			continue
		}
		if err = w.Store.Delivered(ctx, events); err != nil {
			return err
		}
	}
	return nil
}
func (w *Worker) deliver(ctx context.Context, d Destination, events []Event) error {
	token, err := w.accessToken(ctx, d)
	if err != nil {
		return err
	}
	payloads := make([]json.RawMessage, 0, len(events))
	for _, event := range events {
		payload, err := payloadFor(d, event)
		if err != nil {
			return err
		}
		payloads = append(payloads, payload)
	}
	body, err := json.Marshal(map[string]any{"events": payloads})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", events[0].EventID)
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return &httpStatusError{Status: resp.StatusCode}
	}
	return validateReceipts(resp.Body, d.Name, events)
}

func payloadFor(d Destination, event Event) (json.RawMessage, error) {
	if d.Name == "TAX_INVOICE_COMMAND" {
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, errors.New("invalid tax command outbox payload")
		}
		payload["event_id"] = event.EventID
		payload["event_type"] = event.EventType
		payload["tenant_id"] = event.TenantID
		payload["occurred_at"] = event.CreatedAt.UTC()
		if _, exists := payload["schema_version"]; !exists {
			payload["schema_version"] = 1
		}
		return json.Marshal(payload)
	}
	if d.Name != "PLATFORM_AUDIT" {
		return event.Payload, nil
	}
	var detail map[string]any
	if err := json.Unmarshal(event.Payload, &detail); err != nil {
		return nil, errors.New("invalid audit outbox payload")
	}
	actor, _ := detail["actor_id"].(string)
	result, _ := detail["result"].(string)
	if result == "" {
		result = "SUCCESS"
	}
	risk, _ := detail["risk_level"].(string)
	if risk == "" {
		risk = "MEDIUM"
	}
	reason, _ := detail["reason_code"].(string)
	payload := map[string]any{
		"event_id": event.EventID, "application_code": d.ApplicationCode,
		"environment_code": d.EnvironmentCode, "actor_type": "USER", "actor_id": actor,
		"occurred_at": event.CreatedAt.UTC(), "action": event.EventType,
		"resource_type": event.AggregateType, "resource_id": event.AggregateID,
		"request_id": strings.ToLower(event.EventID), "trace_id": strings.ToLower(event.EventID),
		"correlation_id": strings.ToLower(event.EventID), "result": result,
		"reason_code": reason, "risk_level": risk, "classification": "INTERNAL",
		"summary": event.EventType, "metadata": detail,
	}
	return json.Marshal(payload)
}

func validateReceipts(reader io.Reader, destination string, events []Event) error {
	var envelope struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(reader, 1<<20)).Decode(&envelope); err != nil || envelope.Code != "OK" {
		return errors.New("invalid delivery receipt envelope")
	}
	var raw []json.RawMessage
	if destination == "PLATFORM_NOTIFICATION" {
		var data struct {
			Receipts []json.RawMessage `json:"receipts"`
		}
		if json.Unmarshal(envelope.Data, &data) != nil {
			return errors.New("invalid notification receipts")
		}
		raw = data.Receipts
	} else {
		if json.Unmarshal(envelope.Data, &raw) != nil {
			return errors.New("invalid delivery receipts")
		}
	}
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		var receipt struct {
			EventID   string `json:"event_id"`
			Status    string `json:"status"`
			Duplicate bool   `json:"duplicate"`
		}
		if json.Unmarshal(item, &receipt) != nil || receipt.EventID == "" {
			return errors.New("malformed delivery receipt")
		}
		status := strings.ToUpper(receipt.Status)
		if status != "ACCEPTED" && status != "DUPLICATE" && status != "PROCESSED" && !receipt.Duplicate {
			return errors.New("delivery receipt is not terminal success")
		}
		seen[receipt.EventID] = true
	}
	for _, event := range events {
		if !seen[event.EventID] {
			return errors.New("delivery receipt is incomplete")
		}
	}
	return nil
}
func (w *Worker) accessToken(ctx context.Context, d Destination) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if value := w.tokens[d.Name]; value.Value != "" && time.Until(value.Expires) > 30*time.Second {
		return value.Value, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {d.Scope}}
	tokenEndpoint := d.TokenEndpoint
	if tokenEndpoint == "" {
		tokenEndpoint = w.TokenEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(d.ClientID, d.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", &httpStatusError{Status: resp.StatusCode}
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.AccessToken == "" || !strings.EqualFold(result.TokenType, "Bearer") || result.ExpiresIn <= 0 || !containsScope(result.Scope, d.Scope) {
		return "", errors.New("invalid OAuth token response")
	}
	w.tokens[d.Name] = cachedToken{result.AccessToken, time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)}
	return result.AccessToken, nil
}
func containsScope(raw, expected string) bool {
	for _, v := range strings.Fields(raw) {
		if v == expected {
			return true
		}
	}
	return false
}

type httpStatusError struct{ Status int }

func (e *httpStatusError) Error() string { return "HTTP " + strconv.Itoa(e.Status) }
