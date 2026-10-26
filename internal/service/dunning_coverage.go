package service

func dunningCoverageGaps(policies []DunningCoveragePolicy) []DunningCoverageGap {
	gaps := []DunningCoverageGap{}
	cursor := 1
	for _, policy := range policies {
		from, to := policy.AgingFromDays, policy.AgingToDays
		if from < 0 || to < from {
			continue
		}
		if from > cursor {
			gaps = append(gaps, DunningCoverageGap{FromDays: cursor, ToDays: from - 1})
		}
		if to >= cursor {
			cursor = to + 1
		}
	}
	gaps = append(gaps, DunningCoverageGap{FromDays: cursor, OpenEnded: true})
	return gaps
}

type DunningCurrencyTotal struct {
	Currency string `json:"currency"`
	Overdue  string `json:"overdue"`
}

type OutboxEvent struct {
	ID               string `json:"id"`
	EventID          string `json:"event_id"`
	Destination      string `json:"destination"`
	EventType        string `json:"event_type"`
	Status           string `json:"status"`
	AttemptCount     int    `json:"attempt_count"`
	AvailableAt      string `json:"available_at"`
	LastErrorCode    string `json:"last_error_code"`
	LastErrorSummary string `json:"last_error_summary"`
	DeadLetteredAt   string `json:"dead_lettered_at,omitempty"`
	CreatedAt        string `json:"created_at"`
}

// ListOutboxEvents 为受控运维暴露失败或死信事件。
// 只有结算管理员可以查看其他用户的投递失败。
