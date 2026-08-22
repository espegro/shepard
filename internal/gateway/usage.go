package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

type Usage struct {
	Requests         uint64    `json:"requests"`
	Errors           uint64    `json:"errors"`
	InputTokens      uint64    `json:"input_tokens"`
	OutputTokens     uint64    `json:"output_tokens"`
	TotalTokens      uint64    `json:"total_tokens"`
	LastRequestAt    time.Time `json:"last_request_at,omitempty"`
	LastUpstreamCode int       `json:"last_upstream_status,omitempty"`
}

// PeriodUsage is an aggregate for one client and model in a UTC day or month.
type PeriodUsage struct {
	Period       string `json:"period"`
	Client       string `json:"client"`
	Model        string `json:"model"`
	Requests     uint64 `json:"requests"`
	Errors       uint64 `json:"errors"`
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	TotalTokens  uint64 `json:"total_tokens"`
}

type usageStore struct {
	db     *sql.DB
	logger *slog.Logger
}

var memoryDBCounter atomic.Uint64

func newUsageStore(path string, logger *slog.Logger) (*usageStore, error) {
	if path == "" {
		path = fmt.Sprintf("file:shepard-memory-%d?mode=memory&cache=shared", memoryDBCounter.Add(1))
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Usage writes are small and serialized. A single connection avoids SQLite
	// writer contention and gives updates a predictable order.
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		`CREATE TABLE IF NOT EXISTS model_usage (
			model TEXT PRIMARY KEY,
			requests INTEGER NOT NULL,
			errors INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			last_request_at INTEGER NOT NULL,
			last_upstream_status INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS daily_usage (
			day TEXT NOT NULL,
			client TEXT NOT NULL,
			model TEXT NOT NULL,
			requests INTEGER NOT NULL,
			errors INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			PRIMARY KEY (day, client, model)
		)`,
		`CREATE INDEX IF NOT EXISTS daily_usage_client_day
			ON daily_usage (client, day)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize usage database: %w", err)
		}
	}
	return &usageStore{db: db, logger: logger}, nil
}

func (s *usageStore) close() error { return s.db.Close() }

func (s *usageStore) record(client, model string, status int, body []byte) {
	s.recordAt(time.Now().UTC(), client, model, status, body)
}

func (s *usageStore) recordAt(recordedAt time.Time, client, model string, status int, body []byte) {
	in, out, total := extractUsage(body)
	errorIncrement := 0
	if status >= 400 {
		errorIncrement = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("begin usage update", "model", model, "error", err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO model_usage (
			model, requests, errors, input_tokens, output_tokens, total_tokens,
			last_request_at, last_upstream_status
		) VALUES (?, 1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(model) DO UPDATE SET
			requests = requests + 1,
			errors = errors + excluded.errors,
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			total_tokens = total_tokens + excluded.total_tokens,
			last_request_at = excluded.last_request_at,
			last_upstream_status = excluded.last_upstream_status`,
		model, errorIncrement, in, out, total, recordedAt.UTC().UnixNano(), status)
	if err != nil {
		s.logger.Error("persist usage", "model", model, "error", err)
		return
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO daily_usage (
			day, client, model, requests, errors, input_tokens, output_tokens,
			total_tokens
		) VALUES (?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(day, client, model) DO UPDATE SET
			requests = requests + 1,
			errors = errors + excluded.errors,
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			total_tokens = total_tokens + excluded.total_tokens`,
		recordedAt.UTC().Format(time.DateOnly), client, model, errorIncrement, in, out, total)
	if err != nil {
		s.logger.Error("persist daily usage", "client", client, "model", model, "error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("commit usage update", "client", client, "model", model, "error", err)
	}
}

func (s *usageStore) snapshot() map[string]Usage {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT model, requests, errors, input_tokens, output_tokens, total_tokens,
		       last_request_at, last_upstream_status
		FROM model_usage ORDER BY model`)
	if err != nil {
		s.logger.Error("read usage", "error", err)
		return map[string]Usage{}
	}
	defer rows.Close()
	result := make(map[string]Usage)
	for rows.Next() {
		var model string
		var usage Usage
		var timestamp int64
		if err := rows.Scan(&model, &usage.Requests, &usage.Errors, &usage.InputTokens,
			&usage.OutputTokens, &usage.TotalTokens, &timestamp, &usage.LastUpstreamCode); err != nil {
			s.logger.Error("scan usage", "error", err)
			continue
		}
		usage.LastRequestAt = time.Unix(0, timestamp).UTC()
		result[model] = usage
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("iterate usage", "error", err)
	}
	return result
}

func (s *usageStore) periodSnapshot(period, client string) ([]PeriodUsage, error) {
	var periodExpression string
	switch period {
	case "day":
		periodExpression = "day"
	case "month":
		periodExpression = "substr(day, 1, 7)"
	default:
		return nil, fmt.Errorf("unsupported usage period %q", period)
	}

	query := `SELECT ` + periodExpression + ` AS period, client, model,
		       SUM(requests), SUM(errors), SUM(input_tokens),
		       SUM(output_tokens), SUM(total_tokens)
		FROM daily_usage`
	var args []any
	if client != "" {
		query += ` WHERE client = ?`
		args = append(args, client)
	}
	query += ` GROUP BY ` + periodExpression + `, client, model
		ORDER BY period, client, model`

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]PeriodUsage, 0)
	for rows.Next() {
		var item PeriodUsage
		if err := rows.Scan(&item.Period, &item.Client, &item.Model, &item.Requests,
			&item.Errors, &item.InputTokens, &item.OutputTokens, &item.TotalTokens); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func extractUsage(body []byte) (uint64, uint64, uint64) {
	// Support both regular JSON responses and OpenAI-style SSE, where the final
	// data event commonly contains the authoritative usage object.
	var best struct {
		PromptTokens     uint64 `json:"prompt_tokens"`
		CompletionTokens uint64 `json:"completion_tokens"`
		InputTokens      uint64 `json:"input_tokens"`
		OutputTokens     uint64 `json:"output_tokens"`
		TotalTokens      uint64 `json:"total_tokens"`
	}
	decode := func(data []byte) {
		var envelope struct {
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(data, &envelope) != nil || len(envelope.Usage) == 0 || bytes.Equal(envelope.Usage, []byte("null")) {
			return
		}
		var candidate = best
		if json.Unmarshal(envelope.Usage, &candidate) == nil {
			best = candidate
		}
	}

	decode(body)
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			decode(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		}
	}
	in := best.InputTokens
	if in == 0 {
		in = best.PromptTokens
	}
	out := best.OutputTokens
	if out == 0 {
		out = best.CompletionTokens
	}
	total := best.TotalTokens
	if total == 0 {
		total = in + out
	}
	return in, out, total
}
