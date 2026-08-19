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
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize usage database: %w", err)
		}
	}
	return &usageStore{db: db, logger: logger}, nil
}

func (s *usageStore) close() error { return s.db.Close() }

func (s *usageStore) record(model string, status int, body []byte) {
	in, out, total := extractUsage(body)
	errorIncrement := 0
	if status >= 400 {
		errorIncrement = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
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
		model, errorIncrement, in, out, total, time.Now().UTC().UnixNano(), status)
	if err != nil {
		s.logger.Error("persist usage", "model", model, "error", err)
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

func extractUsage(body []byte) (uint64, uint64, uint64) {
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
