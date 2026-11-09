package outbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrLeaseLost = errors.New("outbox lease lost")
var ErrLicensePaused = errors.New("commercial license paused business delivery")

// drainBatchTimeout 是停止信号到达后为“已在途批次”预留的收尾预算：
// 领取后的读取、投递回执校验与状态落库必须在该预算内完成，避免停机打断落库
// 后把已投递事件留成 PROCESSING 等待租约过期（worker.go 领取租约 30s）。
const drainBatchTimeout = 5 * time.Second

// eventStore 是 Worker 依赖的最小出站存储契约。
// 生产装配始终是 *Store；单独抽出接口只为让排空语义测试注入可断言的实现。
type eventStore interface {
	Acquire(ctx context.Context, destination string, limit int) ([]Event, error)
	Delivered(ctx context.Context, events []Event) error
	Failed(ctx context.Context, events []Event, code, summary string, permanent bool) error
	filterImportNotifications(ctx context.Context, events []Event) ([]Event, error)
}

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

// PendingByDestination 按目的地聚合尚未投递成功的事件数（BIZ-XAPI-10）。
// 参数：ctx 为查询上下文。
// 返回值：目的地→未完成事件数；查询失败返回错误，由调用方决定降级或失败。
// 用途：/readyz、/metrics 与 worker 积压告警必须能看到“没有目的地认领”的行，
// 否则凭据缺失只会表现为静默 PENDING。
func (s *Store) PendingByDestination(ctx context.Context) (map[string]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT destination,COUNT(*) FROM settlement_outbox_event WHERE status IN ('PENDING','RETRY_WAIT','PROCESSING') GROUP BY destination`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pending := make(map[string]int64)
	for rows.Next() {
		var destination string
		var count int64
		if err := rows.Scan(&destination, &count); err != nil {
			return nil, err
		}
		pending[destination] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pending, nil
}

// Delivered 原子标记出站成功与税控确认受理，不覆盖已完成的税控结果。
// 参数：ctx为落库预算，events为已收到完整可信回执的本轮事件。
// 返回值：租约丢失或持久化错误，失败保留可恢复状态。
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
				_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_issue_attempt SET status='ACCEPTED',accepted_at=COALESCE(accepted_at,UTC_TIMESTAMP(3)),next_reconcile_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 1 MINUTE) WHERE id=? AND tenant_id=? AND status IN ('PENDING','UNKNOWN')`, payload.AttemptID, e.TenantID)
			} else if payload.RedFlushAttemptID != "" {
				_, err = tx.ExecContext(ctx, `UPDATE settlement_invoice_red_flush_attempt SET status='ACCEPTED',accepted_at=COALESCE(accepted_at,UTC_TIMESTAMP(3)),next_reconcile_at=DATE_ADD(UTC_TIMESTAMP(3),INTERVAL 1 MINUTE) WHERE id=? AND tenant_id=? AND status IN ('PENDING','UNKNOWN')`, payload.RedFlushAttemptID, e.TenantID)
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
	CheckBusinessLicense func(context.Context) error
	Store                *Store
	HTTP                 *http.Client
	TokenEndpoint        string
	BatchSize            int
	Destinations         []Destination
	// DegradedDestinations 是启动期因凭据缺失未装配的集成目的地（BIZ-XAPI-10 告警放行）。
	DegradedDestinations []DegradedDestination
	// UnconfiguredPending 是最近一次扫描发现的“有积压但没有目的地认领”的事件数。
	UnconfiguredPending map[string]int64
	lastBacklogWarning  time.Time
	mu                  sync.Mutex
	tokens              map[string]cachedToken
	// testStore 仅用于测试注入存储实现；为 nil 时走生产 Store 字段。
	testStore eventStore
}

// eventStoreHandle 返回本次投递使用的存储实现；生产路径始终是 Worker.Store。
func (w *Worker) eventStoreHandle() eventStore {
	if w.testStore != nil {
		return w.testStore
	}
	if w.Store == nil {
		return nil
	}
	return w.Store
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
	// 降级目的地只在启动时告警一次，避免每 2 秒刷屏；积压计数随后按扫描刷新。
	for _, degraded := range w.DegradedDestinations {
		slog.Warn("settlement integration destination is not configured; its outbox events remain pending locally",
			"destination", degraded.Name, "reason", degraded.Reason)
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := w.refreshBacklog(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("settlement outbox backlog scan failed; pending counts may be stale", "error", err)
		}
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

// refreshBacklog 统计未装配目的地的积压并在数量为正时限频告警（BIZ-XAPI-10）。
// 参数：ctx 为查询上下文。
// 返回值：查询/扫描失败返回错误；调用方保持投递循环继续，计数留在上一次快照。
func (w *Worker) refreshBacklog(ctx context.Context) error {
	if w.Store == nil || w.Store.DB == nil {
		return nil
	}
	pending, err := w.Store.PendingByDestination(ctx)
	if err != nil {
		return err
	}
	w.UnconfiguredPending = UnconfiguredPending(pending, w.Destinations)
	var total int64
	for _, count := range w.UnconfiguredPending {
		total += count
	}
	if total > 0 && (w.lastBacklogWarning.IsZero() || time.Since(w.lastBacklogWarning) >= time.Minute) {
		w.lastBacklogWarning = time.Now()
		slog.Warn("settlement outbox events have no configured destination and remain pending locally",
			"pending", total, "destinations", w.UnconfiguredPending)
	}
	return nil
}

// RunOnce 认领并投递事件，在发送前执行许可和历史财务放行门禁。
// 参数：ctx为工作器上下文。
// 返回值：存储或投递状态记录错误；已阻止的旧提醒不发送。
// 排空契约：ctx 取消后不再领取新批次，已在途批次改用脱离取消的独立预算上下文
// 完成投递与落库（日志标记 "draining outbox batch"），避免停机打断状态写回。
func (w *Worker) RunOnce(ctx context.Context) error {
	store := w.eventStoreHandle()
	if store == nil {
		return nil
	}
	for _, destination := range w.Destinations {
		if ctx.Err() != nil {
			slog.Info("settlement outbox drain: stop acquiring new batches after shutdown signal",
				"destination", destination.Name)
			return nil
		}
		if destination.Name != "PLATFORM_AUDIT" && destination.Name != "PLATFORM_NOTIFICATION" && w.CheckBusinessLicense != nil {
			if w.CheckBusinessLicense(ctx) != nil {
				continue
			}
		}
		events, err := store.Acquire(ctx, destination.Name, w.BatchSize)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			continue
		}
		// 已领取的批次视为在途：此时若收到停止信号，仍要完成发送与落库
		// （租约 30s 内必须写回），因此读取、投递和状态写回一律使用
		// 脱离取消的独立预算上下文。
		batchCtx, cancelBatch := context.WithTimeout(context.WithoutCancel(ctx), drainBatchTimeout)
		draining := ctx.Err() != nil
		if draining {
			slog.Info("draining outbox batch", "destination", destination.Name, "events", len(events))
		}
		err = w.deliverBatch(batchCtx, destination, events, store)
		cancelBatch()
		if !draining && ctx.Err() != nil && err == nil {
			slog.Info("draining outbox batch", "destination", destination.Name, "events", len(events), "completed_after_signal", true)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// deliverBatch 在给定上下文内完成单个已领取批次的投递与状态落库。
// 参数：ctx 为排空预算上下文（不随停机取消），destination/events 为在途批次，
// store 为存储实现。
// 返回值：需要中止本次扫描的存储错误；投递失败已按原语义记录为可重试/死信。
func (w *Worker) deliverBatch(ctx context.Context, destination Destination, events []Event, store eventStore) error {
	filtered, err := store.filterImportNotifications(ctx, events)
	if err != nil {
		return err
	}
	if len(filtered) == 0 {
		return nil
	}
	if destination.Name == "TAX_INVOICE_COMMAND" {
		return w.deliverTaxEvents(ctx, destination, filtered, store)
	}
	if err = w.deliver(ctx, destination, filtered); err != nil {
		if errors.Is(err, ErrLicensePaused) {
			return nil
		}
		permanent := false
		var status *httpStatusError
		if errors.As(err, &status) {
			permanent = status.Status == 400 || status.Status == 413 || status.Status == 422
		}
		_ = store.Failed(ctx, filtered, "DELIVERY_FAILED", err.Error(), permanent)
		return nil
	}
	return store.Delivered(ctx, filtered)
}

// deliver 核验许可和凭据，保存税控发送意图后校验完整投递回执。
// 参数：ctx为批次预算，d为目的地，events为当前有效租约内事件。
// 返回值：发送、回执或保护事务错误，不把未知结果伪装为未受理。
func (w *Worker) deliver(ctx context.Context, d Destination, events []Event) error {
	if d.Name != "PLATFORM_AUDIT" && d.Name != "PLATFORM_NOTIFICATION" && w.CheckBusinessLicense != nil {
		if w.CheckBusinessLicense(ctx) != nil {
			return ErrLicensePaused
		}
	}
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
	if d.Name != "PLATFORM_AUDIT" && d.Name != "PLATFORM_NOTIFICATION" && w.CheckBusinessLicense != nil {
		if w.CheckBusinessLicense(ctx) != nil {
			return ErrLicensePaused
		}
	}
	if d.Name == "TAX_INVOICE_COMMAND" {
		recorder, ok := w.eventStoreHandle().(taxDispatchRecorder)
		if !ok {
			return errors.Join(errTaxDispatchProtection, errors.New("tax dispatch persistence is not configured"))
		}
		if err = recorder.BeginTaxDispatch(ctx, events); err != nil {
			return errors.Join(errTaxDispatchProtection, err)
		}
	}
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
	// AUD-IP-2026：把业务事务内记录的三类来源地址提升为平台协议字段。
	// 缺少这些字段时平台会退回投递连接地址，正是要修掉的口径。
	loginIP, _ := detail["login_ip"].(string)
	clientIP, _ := detail["client_ip"].(string)
	ipSource, _ := detail["ip_source"].(string)
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
	if loginIP != "" {
		payload["login_ip"] = loginIP
	}
	if clientIP != "" {
		payload["client_ip"] = clientIP
	}
	if ipSource != "" {
		payload["ip_source"] = ipSource
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
