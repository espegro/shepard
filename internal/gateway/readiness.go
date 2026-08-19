package gateway

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"time"

	"shepard/internal/config"
)

func (g *Gateway) ready(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
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
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
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
	code := http.StatusOK
	state := "ready"
	if !ready {
		code = http.StatusServiceUnavailable
		state = "not_ready"
	}
	writeJSON(w, code, map[string]any{"status": state, "providers": status})
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
