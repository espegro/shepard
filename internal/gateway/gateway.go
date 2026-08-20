package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"shepard/internal/config"
)

const usageCaptureLimit = 2 << 20

type Gateway struct {
	config      atomic.Pointer[config.Config]
	client      *http.Client
	logger      *slog.Logger
	usage       *usageStore
	discovery   *discoveryStore
	limits      *limitStore
	queue       *requestQueue
	metrics     metrics
	acl         atomic.Pointer[clientACL]
	trusted     atomic.Pointer[clientACL]
	usageDBPath string
}

func New(cfg *config.Config, logger *slog.Logger) (*Gateway, error) {
	usage, err := newUsageStore(cfg.Server.UsageDB, logger)
	if err != nil {
		return nil, err
	}
	acl, err := newClientACL(cfg.Server.ClientNetworks)
	if err != nil {
		_ = usage.close()
		return nil, err
	}
	trusted, err := newClientACL(cfg.Server.TrustedProxyNetworks)
	if err != nil {
		_ = usage.close()
		return nil, err
	}
	queueSize := cfg.Server.Queue.MaxSize
	if cfg.Server.Queue.Enabled == nil || !*cfg.Server.Queue.Enabled {
		queueSize = 0
	}
	g := &Gateway{
		client:      &http.Client{Transport: http.DefaultTransport},
		logger:      logger,
		usage:       usage,
		discovery:   newDiscoveryStore(),
		limits:      newLimitStore(),
		queue:       newRequestQueue(queueSize),
		usageDBPath: cfg.Server.UsageDB,
	}
	g.config.Store(cfg)
	g.acl.Store(acl)
	if len(cfg.Server.TrustedProxyNetworks) > 0 {
		g.trusted.Store(trusted)
	}
	return g, nil
}

func (g *Gateway) Reload(cfg *config.Config) error {
	if cfg.Server.UsageDB != g.usageDBPath {
		return errors.New("usage_db cannot be changed by reload; restart Shepard")
	}
	acl, err := newClientACL(cfg.Server.ClientNetworks)
	if err != nil {
		return err
	}
	trusted, err := newClientACL(cfg.Server.TrustedProxyNetworks)
	if err != nil {
		return err
	}
	g.config.Store(cfg)
	g.acl.Store(acl)
	if len(cfg.Server.TrustedProxyNetworks) > 0 {
		g.trusted.Store(trusted)
	} else {
		g.trusted.Store(nil)
	}
	queueSize := cfg.Server.Queue.MaxSize
	if cfg.Server.Queue.Enabled == nil || !*cfg.Server.Queue.Enabled {
		queueSize = 0
	}
	g.queue.setMax(queueSize)
	g.discovery.clear()
	g.limits.clear()
	return nil
}

func (g *Gateway) Close() error { return g.usage.close() }

func (g *Gateway) admitWithQueue(ctx context.Context, timeout time.Duration, scopes ...admissionScope) (func(), bool, bool) {
	return g.queue.wait(ctx, timeout, func() (func(), bool) {
		return g.limits.admit(scopes...)
	})
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := g.config.Load()
	clientIP := forwardedClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), g.trusted.Load())
	if acl := g.acl.Load(); acl != nil && !acl.contains(clientIP) {
		g.logger.Warn("client rejected by network ACL", "remote_addr", r.RemoteAddr, "client_addr", clientIPString(clientIP), "x_forwarded_for", r.Header.Get("X-Forwarded-For"))
		writeAPIError(w, http.StatusForbidden, "client network is not allowed")
		return
	}
	if r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/readyz" {
		g.ready(w, r, cfg)
		return
	}
	if !authorized(r, cfg.Server.InboundAPIKeys) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/opencode.json":
		g.opencodeConfig(w, r, cfg)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		g.models(w, r, cfg)
	case r.Method == http.MethodGet && r.URL.Path == "/_shepard/usage":
		writeJSON(w, http.StatusOK, map[string]any{"models": g.usage.snapshot()})
	case r.Method == http.MethodGet && r.URL.Path == "/_shepard/metrics":
		g.metrics.writePrometheus(w, g.queue.depth())
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/responses"):
		g.proxy(w, r, cfg)
	default:
		writeAPIError(w, http.StatusNotFound, "route not found")
	}
}

// opencodeConfig returns a config that points OpenCode at this Shepard
// instance. It intentionally never includes configured inbound or provider
// credentials; those are secrets and must be supplied separately by the
// caller/environment.
func (g *Gateway) opencodeConfig(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	modelIDs := make(map[string]struct{}, len(cfg.Models))
	for alias := range cfg.Models {
		modelIDs[alias] = struct{}{}
	}
	discovered, errs := g.discoverAll(ctx, cfg)
	for provider, err := range errs {
		g.logger.Warn("model autodiscovery failed while generating OpenCode config", "provider", provider, "error", err)
	}
	for _, model := range discovered {
		modelIDs[model.Alias] = struct{}{}
	}

	models := make(map[string]map[string]any, len(modelIDs))
	aliases := make([]string, 0, len(modelIDs))
	for alias := range modelIDs {
		aliases = append(aliases, alias)
		models[alias] = map[string]any{"name": alias}
	}
	sort.Strings(aliases)
	if len(aliases) == 0 {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no models are configured"})
		return
	}

	baseURL := requestBaseURL(r) + "/v1"
	writeJSON(w, http.StatusOK, map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   "shepard/" + aliases[0],
		"provider": map[string]any{
			"shepard": map[string]any{
				"npm":     "@ai-sdk/openai-compatible",
				"name":    "Shepard",
				"options": map[string]any{"baseURL": baseURL},
				"models":  models,
			},
		},
	})
}

func requestBaseURL(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	return scheme + "://" + r.Host
}

func authorized(r *http.Request, keys []string) bool {
	if len(keys) == 0 {
		return true
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	given := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	for _, expected := range keys {
		if subtle.ConstantTimeCompare([]byte(given), []byte(expected)) == 1 {
			return true
		}
	}
	return false
}

func (g *Gateway) models(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	aliases := make([]string, 0, len(cfg.Models))
	for alias := range cfg.Models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	data := make([]map[string]any, 0, len(aliases))
	seen := make(map[string]bool, len(aliases))
	for _, alias := range aliases {
		data = append(data, map[string]any{"id": alias, "object": "model", "owned_by": "shepard"})
		seen[alias] = true
	}
	ctx, cancel := discoveryContext(r.Context(), cfg)
	defer cancel()
	discovered, errs := g.discoverAll(ctx, cfg)
	for _, item := range discovered {
		if seen[item.Alias] {
			continue
		}
		data = append(data, map[string]any{"id": item.Alias, "object": "model", "owned_by": item.Provider})
		seen[item.Alias] = true
	}
	for provider, err := range errs {
		g.logger.Warn("model autodiscovery failed", "provider", provider, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	started := time.Now()
	g.metrics.requests.Add(1)
	g.metrics.active.Add(1)
	completed := false
	defer func() {
		g.metrics.active.Add(-1)
		g.metrics.durationNanos.Add(uint64(time.Since(started)))
		if !completed {
			g.metrics.failed.Add(1)
		}
	}()
	ctx := r.Context()
	if cfg.Server.RequestTimeout.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Server.RequestTimeout.Duration)
		defer cancel()
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cfg.Server.MaxRequestBytes))
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	alias, ok := payload["model"].(string)
	if !ok || alias == "" {
		writeAPIError(w, http.StatusBadRequest, "model must be a string")
		return
	}
	logCfg := newConfigLogging(cfg.Server.Logging)
	g.logRequest(&logCfg, r, alias, body)
	model, ok := cfg.Models[alias]
	if !ok {
		model, ok, err = g.resolveDiscoveredModel(ctx, cfg, alias)
		if err != nil {
			g.logger.Error("model autodiscovery failed", "model", alias, "error", err)
			writeAPIError(w, http.StatusBadGateway, "could not query provider models")
			return
		}
		if !ok {
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("unknown model %q", alias))
			return
		}
	}
	releaseRequest, admitted, queued := g.admitWithQueue(ctx, cfg.Server.Queue.WaitTimeout.Duration,
		admissionScope{key: "client:" + clientIdentity(r), limits: cfg.Server.ClientLimits},
		admissionScope{key: "model:" + alias, limits: model.Limits},
	)
	if queued {
		g.metrics.queueWaits.Add(1)
	}
	if !admitted {
		g.metrics.queueRejected.Add(1)
		g.usage.record(alias, http.StatusTooManyRequests, nil)
		writeRateLimitError(w)
		return
	}
	defer releaseRequest()

	if err := applySystemPrompt(payload, r.URL.Path, model.PrependSystemPrompt, model.AppendSystemPrompt); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	applyRequestOverrides(payload, model.Overrides)
	var resp *http.Response
	var selected config.TargetConfig
	var releaseProvider func()
	var lastErr error
	providerLimited := false
	targets := model.ResolvedTargets()

targetLoop:
	for targetIndex, candidate := range targets {
		provider := cfg.Providers[candidate.Provider]
		targetPayload := clonePayload(payload)
		targetPayload["model"] = candidate.Model
		applyProviderCompatibility(targetPayload, r.URL.Path, provider.Protocol)
		requestBody, marshalErr := json.Marshal(targetPayload)
		if marshalErr != nil {
			writeAPIError(w, http.StatusInternalServerError, "could not encode upstream request")
			return
		}
		for attempt := 0; attempt <= model.Retries; attempt++ {
			providerRelease, allowed, queued := g.admitWithQueue(ctx, cfg.Server.Queue.WaitTimeout.Duration, admissionScope{
				key: "provider:" + candidate.Provider, limits: provider.Limits,
			})
			if queued {
				g.metrics.queueWaits.Add(1)
			}
			if !allowed {
				providerLimited = true
				g.logger.Warn("provider rate limit reached", "model", alias, "provider", candidate.Provider)
				continue targetLoop
			}
			attemptStarted := time.Now()
			attemptCtx := ctx
			var cancelAttempt context.CancelFunc
			if provider.RequestTimeout.Duration > 0 {
				attemptCtx, cancelAttempt = context.WithTimeout(ctx, provider.RequestTimeout.Duration)
			}
			candidateResp, requestErr := g.doUpstreamRequest(attemptCtx, r, provider, requestBody)
			attemptTimedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
			if cancelAttempt != nil {
				cancelAttempt()
			}
			g.metrics.upstreamRequests.Add(1)
			if requestErr != nil {
				g.metrics.upstreamErrors.Add(1)
				providerRelease()
				lastErr = requestErr
				g.logger.Warn("provider attempt failed", "model", alias, "provider", candidate.Provider, "upstream_model", candidate.Model, "attempt", attempt+1, "error", requestErr)
				if ctx.Err() != nil {
					break targetLoop
				}
				if provider.RequestTimeout.Duration > 0 && attemptTimedOut {
					continue targetLoop
				}
				if attempt < model.Retries {
					if !waitForRetry(ctx, attempt) {
						break targetLoop
					}
					continue
				}
				continue targetLoop
			}

			retryable := retryableStatus(candidateResp.StatusCode)
			hasAnotherTarget := targetIndex < len(targets)-1
			shouldRetrySame := retryable && candidateResp.StatusCode != http.StatusTooManyRequests && candidateResp.StatusCode != http.StatusNotFound && attempt < model.Retries
			shouldFailover := retryable && !shouldRetrySame && hasAnotherTarget
			if shouldRetrySame || shouldFailover {
				_, _ = io.Copy(io.Discard, io.LimitReader(candidateResp.Body, 64<<10))
				_ = candidateResp.Body.Close()
				providerRelease()
				g.logger.Warn("retryable provider response", "model", alias, "provider", candidate.Provider, "upstream_model", candidate.Model, "status", candidateResp.StatusCode, "attempt", attempt+1, "duration_ms", time.Since(attemptStarted).Milliseconds())
				if shouldRetrySame {
					if !waitForRetry(ctx, attempt) {
						break targetLoop
					}
					continue
				}
				continue targetLoop
			}

			resp = candidateResp
			selected = candidate
			releaseProvider = providerRelease
			break targetLoop
		}
	}

	if resp == nil {
		status := http.StatusBadGateway
		message := "all provider targets failed"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			message = "provider request timed out"
		} else if providerLimited && lastErr == nil {
			status = http.StatusTooManyRequests
			message = "all provider targets are rate limited"
			w.Header().Set("Retry-After", "1")
		}
		g.usage.record(alias, status, nil)
		writeAPIError(w, status, message)
		return
	}
	defer resp.Body.Close()
	defer releaseProvider()
	g.metrics.completed.Add(1)
	completed = true
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	capture := &tailBuffer{limit: usageCaptureLimit}
	var responseLog *logBodyBuffer
	responseReader := io.Reader(resp.Body)
	if logCfg.enabled && logCfg.responses && logCfg.includeBodies {
		responseLog = newLogBodyBuffer(logCfg.maxBodyBytes)
		responseReader = io.TeeReader(responseReader, responseLog)
	}
	responseReader = io.TeeReader(responseReader, capture)
	destination := io.Writer(w)
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		destination = flushWriter{writer: w}
	}
	_, copyErr := io.Copy(destination, responseReader)
	g.usage.record(alias, resp.StatusCode, capture.Bytes())
	g.logResponse(&logCfg, resp, selected.Provider, selected.Model, responseLog)
	g.logger.Info("request complete", "model", alias, "provider", selected.Provider, "upstream_model", selected.Model, "status", resp.StatusCode, "duration_ms", time.Since(started).Milliseconds(), "remote_addr", r.RemoteAddr, "client_addr", clientIPString(forwardedClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), g.trusted.Load())), "x_forwarded_for", r.Header.Get("X-Forwarded-For"))
	if copyErr != nil {
		g.logger.Error("stream provider response", "model", alias, "error", copyErr)
	}
}

func applyRequestOverrides(payload map[string]any, overrides map[string]any) {
	for key, value := range overrides {
		payload[key] = value
	}
}

func clonePayload(payload map[string]any) map[string]any {
	clone := make(map[string]any, len(payload))
	for key, value := range payload {
		clone[key] = value
	}
	return clone
}

func applyProviderCompatibility(payload map[string]any, endpoint, protocol string) {
	if protocol == "ollama" {
		if value, ok := payload["thinking"]; ok {
			// Ollama's OpenAI-compatible /v1 endpoint uses
			// reasoning_effort, while the native /api endpoints use think.
			// A boolean is mapped to the closest OpenAI-compatible value.
			if _, exists := payload["reasoning_effort"]; !exists {
				switch v := value.(type) {
				case bool:
					if v {
						payload["reasoning_effort"] = "medium"
					} else {
						payload["reasoning_effort"] = "none"
					}
				case string:
					payload["reasoning_effort"] = v
				}
			}
			delete(payload, "thinking")
		}
		if value, ok := payload["think"]; ok {
			if _, exists := payload["reasoning_effort"]; !exists {
				switch v := value.(type) {
				case bool:
					if v {
						payload["reasoning_effort"] = "medium"
					} else {
						payload["reasoning_effort"] = "none"
					}
				case string:
					payload["reasoning_effort"] = v
				}
			}
			delete(payload, "think")
		}
	}
	if endpoint == "/v1/responses" {
		if value, ok := payload["max_tokens"]; ok {
			if _, exists := payload["max_output_tokens"]; !exists {
				payload["max_output_tokens"] = value
			}
			delete(payload, "max_tokens")
		}
	}
}

func (g *Gateway) doUpstreamRequest(ctx context.Context, incoming *http.Request, provider config.ProviderConfig, body []byte) (*http.Response, error) {
	target, err := upstreamURL(provider.BaseURL, incoming.URL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req.Header, incoming.Header)
	req.Header.Set("Content-Type", "application/json")
	if err := applyProviderHeaders(req.Header, provider); err != nil {
		return nil, err
	}
	return g.client.Do(req)
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusNotFound, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func waitForRetry(ctx context.Context, attempt int) bool {
	delay := 100 * time.Millisecond * time.Duration(1<<min(attempt, 4))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func writeRateLimitError(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeAPIError(w, http.StatusTooManyRequests, "rate or concurrency limit exceeded")
}

func discoveryContext(parent context.Context, cfg *config.Config) (context.Context, context.CancelFunc) {
	if cfg.Server.RequestTimeout.Duration <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, cfg.Server.RequestTimeout.Duration)
}

func applyProviderHeaders(header http.Header, provider config.ProviderConfig) error {
	if provider.APIKeyEnv != "" {
		key := os.Getenv(provider.APIKeyEnv)
		if key == "" {
			return fmt.Errorf("environment variable %s is empty", provider.APIKeyEnv)
		}
		header.Set("Authorization", "Bearer "+key)
	} else {
		header.Del("Authorization")
	}
	for name, value := range provider.Headers {
		header.Set(name, value)
	}
	return nil
}

func upstreamURL(base string, incoming *url.URL) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	rel := strings.TrimPrefix(incoming.Path, "/v1/")
	u.Path = path.Join(strings.TrimSuffix(u.Path, "/"), rel)
	u.RawQuery = incoming.RawQuery
	return u.String(), nil
}

func applySystemPrompt(payload map[string]any, endpoint, prepend, append string) error {
	if prepend == "" && append == "" {
		return nil
	}
	if endpoint == "/v1/responses" {
		current, _ := payload["instructions"].(string)
		payload["instructions"] = joinPrompt(prepend, current, append)
		return nil
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return errors.New("messages must be an array")
	}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "system" {
			continue
		}
		content, ok := message["content"].(string)
		if !ok {
			// Multimodal system content is valid for OpenAI-compatible APIs.
			// Keep it intact and add the configured text as a separate system
			// message below instead of rejecting the whole request.
			continue
		}
		message["content"] = joinPrompt(prepend, content, append)
		payload["messages"] = messages
		return nil
	}
	prompt := joinPrompt(prepend, "", append)
	payload["messages"] = appendAny([]any{map[string]any{"role": "system", "content": prompt}}, messages...)
	return nil
}

func joinPrompt(parts ...string) string {
	nonempty := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			nonempty = append(nonempty, p)
		}
	}
	return strings.Join(nonempty, "\n\n")
}

func appendAny(head []any, tail ...any) []any { return append(head, tail...) }

func copyRequestHeaders(dst, src http.Header) {
	for name, values := range src {
		switch strings.ToLower(name) {
		case "authorization", "host", "content-length", "connection":
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		switch strings.ToLower(name) {
		case "content-length", "connection":
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSONStatus(w, status, map[string]any{"error": map[string]any{"message": message, "type": "shepard_error"}})
}

func writeJSON(w http.ResponseWriter, status int, value any) { writeJSONStatus(w, status, value) }

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type tailBuffer struct {
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return original, nil
	}
	overflow := len(b.buf) + len(p) - b.limit
	if overflow > 0 {
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:len(b.buf)-overflow]
	}
	b.buf = append(b.buf, p...)
	return original, nil
}

func (b *tailBuffer) Bytes() []byte { return b.buf }

type flushWriter struct{ writer http.ResponseWriter }

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if flusher, ok := w.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}
