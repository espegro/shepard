package gateway

import (
	"bytes"
	"net/http"
	"strings"

	"shepard/internal/config"
)

var sensitiveHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
}

func loggedHeaders(header http.Header) map[string]string {
	result := make(map[string]string, len(header))
	for name, values := range header {
		value := strings.Join(values, ", ")
		if _, sensitive := sensitiveHeaders[strings.ToLower(name)]; sensitive {
			value = "[REDACTED]"
		}
		result[name] = value
	}
	return result
}

func (g *Gateway) logRequest(cfg *configLogging, r *http.Request, model string, body []byte) {
	if !cfg.enabled || !cfg.requests {
		return
	}
	attrs := []any{"method", r.Method, "path", r.URL.Path, "model", model, "headers", loggedHeaders(r.Header)}
	if cfg.includeBodies {
		value, truncated := boundedLogBody(body, cfg.maxBodyBytes)
		attrs = append(attrs, "body", value, "body_truncated", truncated)
	}
	g.logger.Info("client request", attrs...)
}

func (g *Gateway) logResponse(cfg *configLogging, resp *http.Response, provider, model string, body *logBodyBuffer) {
	if !cfg.enabled || !cfg.responses {
		return
	}
	attrs := []any{"status", resp.StatusCode, "provider", provider, "upstream_model", model, "headers", loggedHeaders(resp.Header)}
	if cfg.includeBodies && body != nil {
		attrs = append(attrs, "body", body.String(), "body_truncated", body.truncated)
	}
	g.logger.Info("provider response", attrs...)
}

type configLogging struct {
	enabled       bool
	requests      bool
	responses     bool
	includeBodies bool
	maxBodyBytes  int64
}

func newConfigLogging(cfg config.LoggingConfig) configLogging {
	return configLogging{enabled: cfg.Enabled, requests: cfg.Requests, responses: cfg.Responses, includeBodies: cfg.IncludeBodies, maxBodyBytes: cfg.MaxBodyBytes}
}

type logBodyBuffer struct {
	bytes.Buffer
	limit     int64
	truncated bool
}

func newLogBodyBuffer(limit int64) *logBodyBuffer { return &logBodyBuffer{limit: limit} }

func (b *logBodyBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.truncated = true
		return len(p), nil
	}
	remaining := b.limit - int64(b.Len())
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func boundedLogBody(body []byte, limit int64) (string, bool) {
	buffer := newLogBodyBuffer(limit)
	_, _ = buffer.Write(body)
	return buffer.String(), buffer.truncated
}
