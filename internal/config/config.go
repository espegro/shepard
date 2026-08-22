package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	d.Duration = v
	return nil
}

// Config separates listener policy, upstream connections, and client-facing
// model aliases so providers can change without changing client configuration.
type Config struct {
	Server    ServerConfig              `yaml:"server"`
	Providers map[string]ProviderConfig `yaml:"providers"`
	Models    map[string]ModelConfig    `yaml:"models"`
}

type ServerConfig struct {
	Listen                string        `yaml:"listen"`
	InboundAPIKeys        []string      `yaml:"inbound_api_keys"`
	UsageDB               string        `yaml:"usage_db"`
	MaxConcurrentRequests int           `yaml:"max_concurrent_requests"`
	ClientLimits          Limits        `yaml:"client_limits"`
	ReadHeaderTimeout     Duration      `yaml:"read_header_timeout"`
	IdleTimeout           Duration      `yaml:"idle_timeout"`
	RequestTimeout        Duration      `yaml:"request_timeout"`
	ReadTimeout           Duration      `yaml:"read_timeout"`
	WriteIdleTimeout      Duration      `yaml:"write_idle_timeout"`
	ReadinessCacheTTL     Duration      `yaml:"readiness_cache_ttl"`
	MaxHeaderBytes        int           `yaml:"max_header_bytes"`
	MaxRequestBytes       int64         `yaml:"max_request_bytes"`
	Queue                 QueueConfig   `yaml:"queue"`
	Logging               LoggingConfig `yaml:"logging"`
	ClientNetworks        []string      `yaml:"client_networks"`
	TrustedProxyNetworks  []string      `yaml:"trusted_proxy_networks"`
}

type QueueConfig struct {
	Enabled     *bool    `yaml:"enabled"`
	MaxSize     int      `yaml:"max_size"`
	WaitTimeout Duration `yaml:"wait_timeout"`
}

type LoggingConfig struct {
	Enabled       bool  `yaml:"enabled"`
	Requests      bool  `yaml:"requests"`
	Responses     bool  `yaml:"responses"`
	IncludeBodies bool  `yaml:"include_bodies"`
	MaxBodyBytes  int64 `yaml:"max_body_bytes"`
}

type ProviderConfig struct {
	BaseURL        string             `yaml:"base_url"`
	Protocol       string             `yaml:"protocol"`
	APIKeyEnv      string             `yaml:"api_key_env"`
	Headers        map[string]string  `yaml:"headers"`
	Autodiscover   AutodiscoverConfig `yaml:"autodiscover"`
	Limits         Limits             `yaml:"limits"`
	RequestTimeout Duration           `yaml:"request_timeout"`
}

type AutodiscoverConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Prefix   string   `yaml:"prefix"`
	CacheTTL Duration `yaml:"cache_ttl"`
}

type ModelConfig struct {
	Provider            string         `yaml:"provider"`
	Model               string         `yaml:"model"`
	Targets             []TargetConfig `yaml:"targets"`
	Retries             int            `yaml:"retries"`
	Limits              Limits         `yaml:"limits"`
	Overrides           map[string]any `yaml:"overrides"`
	PrependSystemPrompt string         `yaml:"prepend_system_prompt"`
	AppendSystemPrompt  string         `yaml:"append_system_prompt"`
}

type TargetConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type Limits struct {
	RequestsPerMinute float64 `yaml:"requests_per_minute"`
	Burst             int     `yaml:"burst"`
	MaxConcurrent     int     `yaml:"max_concurrent"`
}

func (m ModelConfig) ResolvedTargets() []TargetConfig {
	// provider/model is the compact form for the common single-target case.
	if len(m.Targets) > 0 {
		return m.Targets
	}
	return []TargetConfig{{Provider: m.Provider, Model: m.Model}}
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	// Reject misspelled settings rather than silently running with defaults.
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	cfg.defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) defaults() {
	// Defaults are applied before validation so validation sees the exact
	// configuration that the gateway will use.
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.ReadHeaderTimeout.Duration == 0 {
		c.Server.ReadHeaderTimeout.Duration = 10 * time.Second
	}
	if c.Server.IdleTimeout.Duration == 0 {
		c.Server.IdleTimeout.Duration = 2 * time.Minute
	}
	if c.Server.RequestTimeout.Duration == 0 {
		c.Server.RequestTimeout.Duration = 10 * time.Minute
	}
	if c.Server.ReadTimeout.Duration == 0 {
		c.Server.ReadTimeout.Duration = 30 * time.Second
	}
	if c.Server.WriteIdleTimeout.Duration == 0 {
		c.Server.WriteIdleTimeout.Duration = 30 * time.Second
	}
	if c.Server.ReadinessCacheTTL.Duration == 0 {
		c.Server.ReadinessCacheTTL.Duration = 3 * time.Second
	}
	if c.Server.MaxHeaderBytes == 0 {
		c.Server.MaxHeaderBytes = 64 << 10
	}
	if c.Server.MaxConcurrentRequests == 0 {
		c.Server.MaxConcurrentRequests = 64
	}
	if c.Server.Queue.MaxSize == 0 {
		c.Server.Queue.MaxSize = 32
	}
	if c.Server.Queue.Enabled == nil {
		enabled := true
		c.Server.Queue.Enabled = &enabled
	}
	if c.Server.Queue.WaitTimeout.Duration == 0 {
		c.Server.Queue.WaitTimeout.Duration = 30 * time.Second
	}
	if c.Server.Logging.MaxBodyBytes == 0 {
		c.Server.Logging.MaxBodyBytes = 64 << 10
	}
	if c.Server.MaxRequestBytes == 0 {
		c.Server.MaxRequestBytes = 8 << 20
	}
	if c.Server.UsageDB == "" {
		c.Server.UsageDB = "shepard-usage.db"
	}
	c.Server.ClientLimits.defaults()
	for name, provider := range c.Providers {
		if provider.Protocol == "" {
			provider.Protocol = "openai"
		}
		provider.Limits.defaults()
		if provider.Autodiscover.Enabled {
			if provider.Autodiscover.Prefix == "" {
				provider.Autodiscover.Prefix = name + "/"
			}
			if provider.Autodiscover.CacheTTL.Duration == 0 {
				provider.Autodiscover.CacheTTL.Duration = time.Minute
			}
			c.Providers[name] = provider
		}
		c.Providers[name] = provider
	}
	for alias, model := range c.Models {
		model.Limits.defaults()
		c.Models[alias] = model
	}
}

func (l *Limits) defaults() {
	if l.RequestsPerMinute > 0 && l.Burst == 0 {
		l.Burst = 1
	}
}

func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return errors.New("at least one provider is required")
	}
	hasModels := len(c.Models) > 0
	prefixes := make(map[string]string)
	for name, p := range c.Providers {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("provider %q has invalid base_url", name)
		}
		if p.APIKeyEnv != "" && strings.Contains(p.APIKeyEnv, "$") {
			return fmt.Errorf("provider %q api_key_env must be an environment variable name, without $", name)
		}
		if p.Autodiscover.Enabled {
			hasModels = true
			if other, exists := prefixes[p.Autodiscover.Prefix]; exists {
				return fmt.Errorf("providers %q and %q use the same autodiscover prefix %q", other, name, p.Autodiscover.Prefix)
			}
			prefixes[p.Autodiscover.Prefix] = name
		}
		if err := p.Limits.validate("provider " + name); err != nil {
			return err
		}
		if p.Protocol != "openai" && p.Protocol != "ollama" {
			return fmt.Errorf("provider %q has unsupported protocol %q", name, p.Protocol)
		}
		if p.RequestTimeout.Duration < 0 {
			return fmt.Errorf("provider %q request_timeout must not be negative", name)
		}
	}
	if err := c.Server.ClientLimits.validate("server client_limits"); err != nil {
		return err
	}
	if c.Server.Queue.MaxSize < 0 {
		return errors.New("server queue max_size must not be negative")
	}
	if c.Server.Queue.WaitTimeout.Duration < 0 {
		return errors.New("server queue wait_timeout must not be negative")
	}
	if c.Server.Logging.MaxBodyBytes < 0 {
		return errors.New("server logging max_body_bytes must not be negative")
	}
	if c.Server.MaxConcurrentRequests < 0 {
		return errors.New("server max_concurrent_requests must not be negative")
	}
	if c.Server.MaxHeaderBytes < 0 {
		return errors.New("server max_header_bytes must not be negative")
	}
	if c.Server.WriteIdleTimeout.Duration < 0 {
		return errors.New("server write_idle_timeout must not be negative")
	}
	if c.Server.ReadinessCacheTTL.Duration < 0 {
		return errors.New("server readiness_cache_ttl must not be negative")
	}
	for _, value := range append(append([]string{}, c.Server.ClientNetworks...), c.Server.TrustedProxyNetworks...) {
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("server client_networks must not contain an empty value")
		}
		if strings.Contains(value, "/") {
			if _, _, err := net.ParseCIDR(value); err != nil {
				return fmt.Errorf("server client_networks contains invalid network %q", value)
			}
			continue
		}
		if net.ParseIP(value) == nil {
			return fmt.Errorf("server client_networks contains invalid IP %q", value)
		}
	}
	if !hasModels {
		return errors.New("at least one static model or autodiscover-enabled provider is required")
	}
	for alias, m := range c.Models {
		if alias == "" {
			return errors.New("model alias must not be empty")
		}
		if len(m.Targets) > 0 && (m.Provider != "" || m.Model != "") {
			return fmt.Errorf("model %q must use either provider/model or targets, not both", alias)
		}
		if len(m.Targets) == 0 && (m.Provider == "" || m.Model == "") {
			return fmt.Errorf("model %q requires provider/model or at least one target", alias)
		}
		if m.Retries < 0 || m.Retries > 10 {
			return fmt.Errorf("model %q retries must be between 0 and 10", alias)
		}
		for key := range m.Overrides {
			switch key {
			case "model", "messages", "input", "instructions":
				return fmt.Errorf("model %q override cannot set %q", alias, key)
			}
		}
		if err := m.Limits.validate("model " + alias); err != nil {
			return err
		}
		for _, target := range m.ResolvedTargets() {
			if target.Model == "" {
				return fmt.Errorf("model %q has a target with an empty model", alias)
			}
			if _, ok := c.Providers[target.Provider]; !ok {
				return fmt.Errorf("model %q refers to unknown provider %q", alias, target.Provider)
			}
		}
	}
	return nil
}

// BroadlyExposedWithoutAccessControls reports configurations that bind a
// wildcard address without either bearer authentication or a client ACL.
// This remains a warning rather than a validation error so intentionally open
// development deployments continue to work.
func (s ServerConfig) BroadlyExposedWithoutAccessControls() bool {
	if len(s.InboundAPIKeys) > 0 || len(s.ClientNetworks) > 0 {
		return false
	}
	host, _, err := net.SplitHostPort(s.Listen)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return host == "" || (ip != nil && ip.IsUnspecified())
}

func (l Limits) validate(scope string) error {
	if l.RequestsPerMinute < 0 || l.Burst < 0 || l.MaxConcurrent < 0 {
		return fmt.Errorf("%s limits must not be negative", scope)
	}
	if l.RequestsPerMinute == 0 && l.Burst != 0 {
		return fmt.Errorf("%s burst requires requests_per_minute", scope)
	}
	return nil
}
