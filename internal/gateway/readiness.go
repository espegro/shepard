package gateway

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"shepard/internal/config"
)

type readinessResult struct {
	status map[string]string
	ready  bool
}

type readinessCache struct {
	mu         sync.Mutex
	result     readinessResult
	expires    time.Time
	inflight   chan struct{}
	generation uint64
}

func newReadinessCache() *readinessCache { return &readinessCache{} }

func (c *readinessCache) clear() {
	c.mu.Lock()
	c.result = readinessResult{}
	c.expires = time.Time{}
	c.generation++
	c.mu.Unlock()
}

func (g *Gateway) ready(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	result, ok := g.cachedReadiness(r.Context(), cfg)
	if !ok {
		writeAPIError(w, http.StatusServiceUnavailable, "readiness check cancelled")
		return
	}
	code := http.StatusOK
	state := "ready"
	if !result.ready {
		code = http.StatusServiceUnavailable
		state = "not_ready"
	}
	writeJSON(w, code, map[string]any{"status": state, "providers": result.status})
}

func (g *Gateway) cachedReadiness(ctx context.Context, cfg *config.Config) (readinessResult, bool) {
	for {
		g.readiness.mu.Lock()
		if !g.readiness.expires.IsZero() && time.Now().Before(g.readiness.expires) {
			result := cloneReadinessResult(g.readiness.result)
			g.readiness.mu.Unlock()
			return result, true
		}
		if inflight := g.readiness.inflight; inflight != nil {
			g.readiness.mu.Unlock()
			select {
			case <-ctx.Done():
				return readinessResult{}, false
			case <-inflight:
				continue
			}
		}
		inflight := make(chan struct{})
		generation := g.readiness.generation
		g.readiness.inflight = inflight
		g.readiness.mu.Unlock()

		result := g.checkReadiness(cfg)
		g.readiness.mu.Lock()
		if generation == g.readiness.generation {
			g.readiness.result = cloneReadinessResult(result)
			g.readiness.expires = time.Now().Add(cfg.Server.ReadinessCacheTTL.Duration)
		}
		g.readiness.inflight = nil
		close(inflight)
		g.readiness.mu.Unlock()
		return result, true
	}
}

func cloneReadinessResult(result readinessResult) readinessResult {
	status := make(map[string]string, len(result.status))
	for name, value := range result.status {
		status[name] = value
	}
	return readinessResult{status: status, ready: result.ready}
}

func (g *Gateway) checkReadiness(cfg *config.Config) readinessResult {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	type result struct {
		name string
		ok   bool
	}
	results := make(chan result, len(names))
	for _, name := range names {
		go func(name string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			results <- result{name: name, ok: g.checkProvider(ctx, cfg.Providers[name])}
		}(name)
	}
	status := make(map[string]string, len(names))
	ready := false
	for range names {
		item := <-results
		if item.ok {
			status[item.name] = "ok"
			ready = true
		} else {
			status[item.name] = "unavailable"
		}
	}
	return readinessResult{status: status, ready: ready}
}

func (g *Gateway) checkProvider(ctx context.Context, provider config.ProviderConfig) bool {
	target, err := upstreamURL(provider.BaseURL, &url.URL{Path: "/v1/models"})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil || applyProviderHeaders(req.Header, provider) != nil {
		return false
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
