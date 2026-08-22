package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"shepard/internal/config"
)

const discoveryResponseLimit = 4 << 20

type discoveredModel struct {
	Alias    string
	Model    string
	Provider string
}

type discoveryEntry struct {
	expires time.Time
	models  []discoveredModel
}

type discoveryStore struct {
	mu      sync.RWMutex
	entries map[string]discoveryEntry
}

func newDiscoveryStore() *discoveryStore {
	return &discoveryStore{entries: make(map[string]discoveryEntry)}
}

func (s *discoveryStore) clear() {
	s.mu.Lock()
	s.entries = make(map[string]discoveryEntry)
	s.mu.Unlock()
}

func (s *discoveryStore) get(provider string) ([]discoveredModel, bool) {
	s.mu.RLock()
	entry, ok := s.entries[provider]
	s.mu.RUnlock()
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.models, true
}

func (s *discoveryStore) put(provider string, ttl time.Duration, models []discoveredModel) {
	s.mu.Lock()
	s.entries[provider] = discoveryEntry{expires: time.Now().Add(ttl), models: models}
	s.mu.Unlock()
}

func (g *Gateway) discoverAll(ctx context.Context, cfg *config.Config) ([]discoveredModel, map[string]error) {
	providers := make([]string, 0, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		if provider.Autodiscover.Enabled {
			providers = append(providers, name)
		}
	}
	sort.Strings(providers)
	var all []discoveredModel
	errs := make(map[string]error)
	for _, name := range providers {
		models, err := g.discoverProvider(ctx, name, cfg.Providers[name])
		if err != nil {
			errs[name] = err
			continue
		}
		all = append(all, models...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Alias < all[j].Alias })
	return all, errs
}

func (g *Gateway) resolveDiscoveredModel(ctx context.Context, cfg *config.Config, alias string) (config.ModelConfig, bool, error) {
	for name, provider := range cfg.Providers {
		if !provider.Autodiscover.Enabled || !strings.HasPrefix(alias, provider.Autodiscover.Prefix) {
			continue
		}
		models, err := g.discoverProvider(ctx, name, provider)
		if err != nil {
			return config.ModelConfig{}, false, err
		}
		for _, model := range models {
			if model.Alias == alias {
				return config.ModelConfig{Provider: name, Model: model.Model}, true, nil
			}
		}
		return config.ModelConfig{}, false, nil
	}
	return config.ModelConfig{}, false, nil
}

func (g *Gateway) discoverProvider(ctx context.Context, name string, provider config.ProviderConfig) ([]discoveredModel, error) {
	// Cache only successful results. Failures remain retryable on the next
	// request instead of hiding an upstream recovery for the full TTL.
	if models, ok := g.discovery.get(name); ok {
		return models, nil
	}
	target, err := upstreamURL(provider.BaseURL, &url.URL{Path: "/v1/models"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if err := applyProviderHeaders(req.Header, provider); err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, discoveryResponseLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > discoveryResponseLimit {
		return nil, fmt.Errorf("model list exceeds %d bytes", discoveryResponseLimit)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models endpoint returned status %d", resp.StatusCode)
	}
	var envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}
	models := make([]discoveredModel, 0, len(envelope.Data))
	for _, item := range envelope.Data {
		if item.ID == "" {
			continue
		}
		models = append(models, discoveredModel{
			// Namespacing prevents providers with identical model IDs from
			// colliding in Shepard's client-facing model list.
			Alias:    provider.Autodiscover.Prefix + item.ID,
			Model:    item.ID,
			Provider: name,
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Alias < models[j].Alias })
	g.discovery.put(name, provider.Autodiscover.CacheTTL.Duration, models)
	return models, nil
}
