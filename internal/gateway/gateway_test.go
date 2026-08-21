package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shepard/internal/config"
)

func TestChatAliasCredentialPromptAndUsage(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "provider-secret")
	var received map[string]any
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer provider-secret" {
			t.Errorf("unexpected authorization %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)),
			Request:    r,
		}, nil
	})

	cfg := testConfig("http://provider.invalid/v1")
	model := cfg.Models["stable"]
	model.Overrides = map[string]any{
		"temperature":       0.2,
		"max_output_tokens": 128,
		"reasoning":         map[string]any{"effort": "low"},
	}
	cfg.Models["stable"] = model
	cfg.Server.InboundAPIKeys = []string{"client-secret"}
	g := mustGateway(t, cfg)
	g.client.Transport = transport

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"stable","messages":[{"role":"system","content":"original"},{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	g.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}
	if received["model"] != "provider-model" {
		t.Fatalf("model was not rewritten: %#v", received["model"])
	}
	if received["temperature"] != 0.2 || received["max_output_tokens"] != float64(128) {
		t.Fatalf("model overrides were not applied: %#v", received)
	}
	if reasoning, ok := received["reasoning"].(map[string]any); !ok || reasoning["effort"] != "low" {
		t.Fatalf("reasoning override was not applied: %#v", received["reasoning"])
	}
	messages := received["messages"].([]any)
	system := messages[0].(map[string]any)["content"]
	if system != "before\n\noriginal\n\nafter" {
		t.Fatalf("unexpected system prompt %#v", system)
	}

	usageReq := httptest.NewRequest(http.MethodGet, "/_shepard/usage", nil)
	usageReq.Header.Set("Authorization", "Bearer client-secret")
	usageResp := httptest.NewRecorder()
	g.ServeHTTP(usageResp, usageReq)
	var snapshot struct {
		Models map[string]Usage `json:"models"`
	}
	if err := json.NewDecoder(usageResp.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Models["stable"]; got.Requests != 1 || got.TotalTokens != 15 || got.InputTokens != 12 || got.OutputTokens != 3 {
		t.Fatalf("unexpected usage: %+v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAuthenticationAndUnknownAlias(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1/v1")
	cfg.Server.InboundAPIKeys = []string{"secret"}
	g := mustGateway(t, cfg)

	unauthorized := httptest.NewRecorder()
	g.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"missing","messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	g.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", response.Code)
	}
}

func TestOpenCodeConfig(t *testing.T) {
	cfg := testConfig("http://provider.invalid/v1")
	g := mustGateway(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "http://shepard.example/opencode.json", nil)
	req.Host = "shepard.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	resp := httptest.NewRecorder()
	g.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}
	var document struct {
		Schema   string `json:"$schema"`
		Model    string `json:"model"`
		Provider map[string]struct {
			NPM     string `json:"npm"`
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
			Models map[string]any `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	provider, ok := document.Provider["shepard"]
	if document.Schema != "https://opencode.ai/config.json" || document.Model != "shepard/stable" || !ok {
		t.Fatalf("unexpected OpenCode config: %+v", document)
	}
	if provider.NPM != "@ai-sdk/openai-compatible" || provider.Options.BaseURL != "https://shepard.example/v1" {
		t.Fatalf("unexpected provider config: %+v", provider)
	}
	if _, ok := provider.Models["stable"]; !ok {
		t.Fatalf("static model missing: %+v", provider.Models)
	}
}

func TestReadinessAndMetricsEndpoints(t *testing.T) {
	cfg := testConfig("http://provider.invalid/v1")
	g := mustGateway(t, cfg)

	ready := httptest.NewRecorder()
	g.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected not ready, got %d: %s", ready.Code, ready.Body.String())
	}

	metrics := httptest.NewRecorder()
	g.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/_shepard/metrics", nil))
	if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), "shepard_requests_total") {
		t.Fatalf("unexpected metrics response: %d %s", metrics.Code, metrics.Body.String())
	}
}

func TestReadinessCachesProviderChecks(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "provider-secret")
	checks := 0
	cfg := testConfig("http://provider.invalid/v1")
	cfg.Server.ReadinessCacheTTL = config.Duration{Duration: time.Minute}
	g := mustGateway(t, cfg)
	g.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		checks++
		return jsonResponse(r, `{"object":"list","data":[]}`), nil
	})

	for range 2 {
		response := httptest.NewRecorder()
		g.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("unexpected readiness status %d: %s", response.Code, response.Body.String())
		}
	}
	if checks != 1 {
		t.Fatalf("provider readiness checks = %d, want 1", checks)
	}
}

func TestHeaderForwardingUsesAllowlists(t *testing.T) {
	source := http.Header{
		"Accept":              []string{"application/json"},
		"Authorization":       []string{"Bearer client-secret"},
		"Cookie":              []string{"session=secret"},
		"Proxy-Authorization": []string{"Basic secret"},
		"X-Api-Key":           []string{"client-key"},
		"X-Request-Id":        []string{"request-123"},
	}
	requestHeaders := make(http.Header)
	copyRequestHeaders(requestHeaders, source)
	if requestHeaders.Get("Accept") == "" || requestHeaders.Get("X-Request-Id") == "" {
		t.Fatalf("safe request headers were not forwarded: %#v", requestHeaders)
	}
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key"} {
		if requestHeaders.Get(name) != "" {
			t.Fatalf("sensitive request header %s was forwarded", name)
		}
	}

	responseHeaders := make(http.Header)
	copyResponseHeaders(responseHeaders, http.Header{
		"Content-Type": []string{"application/json"},
		"Set-Cookie":   []string{"session=provider"},
		"Server":       []string{"provider-internal"},
	})
	if responseHeaders.Get("Content-Type") == "" || responseHeaders.Get("Set-Cookie") != "" || responseHeaders.Get("Server") != "" {
		t.Fatalf("unexpected forwarded response headers: %#v", responseHeaders)
	}
}

func TestProviderTimeoutRemainsActiveWhileReadingBody(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "provider-secret")
	cfg := testConfig("http://provider.invalid/v1")
	provider := cfg.Providers["test"]
	provider.RequestTimeout = config.Duration{Duration: time.Second}
	cfg.Providers["test"] = provider
	g := mustGateway(t, cfg)
	g.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       &delayedContextBody{ctx: r.Context(), data: []byte(`{"ok":true}`)},
			Request:    r,
		}, nil
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"stable","messages":[]}`))
	response := httptest.NewRecorder()
	g.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{"ok":true}` {
		t.Fatalf("provider body was cancelled early: status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestGlobalIngressLimitAppliesBeforeReadingBody(t *testing.T) {
	cfg := testConfig("http://provider.invalid/v1")
	cfg.Server.MaxConcurrentRequests = 1
	g := mustGateway(t, cfg)

	reader := &blockingReader{entered: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader))
	}()
	<-reader.entered

	second := httptest.NewRecorder()
	g.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"stable","messages":[]}`)))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", second.Code)
	}
	close(reader.release)
	<-firstDone
}

type blockingReader struct {
	entered chan struct{}
	release chan struct{}
	once    bool
}

func (r *blockingReader) Read([]byte) (int, error) {
	if !r.once {
		r.once = true
		close(r.entered)
	}
	<-r.release
	return 0, io.EOF
}

type delayedContextBody struct {
	ctx  interface{ Done() <-chan struct{} }
	data []byte
	done bool
}

func (b *delayedContextBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	select {
	case <-b.ctx.Done():
		return 0, errors.New("request context cancelled before response body was read")
	case <-time.After(10 * time.Millisecond):
	}
	b.done = true
	return copy(p, b.data), nil
}

func (b *delayedContextBody) Close() error { return nil }

func TestClientNetworkACLSupportsIPv4AndIPv6(t *testing.T) {
	acl, err := newClientACL([]string{"192.0.2.10", "2001:db8:1234::/48"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		remote string
		want   bool
	}{
		{remote: "192.0.2.10:1234", want: true},
		{remote: "192.0.2.11:1234", want: false},
		{remote: "[2001:db8:1234::42]:443", want: true},
		{remote: "[2001:db8:5678::42]:443", want: false},
	} {
		if got := acl.allowed(test.remote); got != test.want {
			t.Errorf("ACL allowed(%q)=%v, want %v", test.remote, got, test.want)
		}
	}
}

func TestForwardedClientIPRequiresTrustedProxy(t *testing.T) {
	trusted, err := newClientACL([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := clientIPString(forwardedClientIP("192.0.2.10:1234", "10.0.0.5", trusted)); got != "192.0.2.10" {
		t.Fatalf("untrusted peer forwarded IP=%s", got)
	}
	if got := clientIPString(forwardedClientIP("127.0.0.1:1234", "10.0.0.5", trusted)); got != "10.0.0.5" {
		t.Fatalf("trusted proxy forwarded IP=%s", got)
	}
}

func TestResponsesInstructions(t *testing.T) {
	payload := map[string]any{"instructions": "original"}
	if err := applySystemPrompt(payload, "/v1/responses", "before", "after"); err != nil {
		t.Fatal(err)
	}
	if payload["instructions"] != "before\n\noriginal\n\nafter" {
		t.Fatalf("unexpected instructions: %#v", payload["instructions"])
	}
}

func TestMultimodalImageContentPassesThrough(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "provider-secret")
	var received map[string]any
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		return jsonResponse(r, `{"choices":[{"message":{"role":"assistant","content":"image received"}}]}`), nil
	})
	cfg := testConfig("http://provider.invalid/v1")
	model := cfg.Models["stable"]
	model.PrependSystemPrompt = "describe the image"
	model.AppendSystemPrompt = "be concise"
	cfg.Models["stable"] = model
	g := mustGateway(t, cfg)
	g.client.Transport = transport

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"stable","messages":[{"role":"system","content":[{"type":"text","text":"system context"}]},{"role":"user","content":[{"type":"text","text":"What is shown?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	g.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
	messages := received["messages"].([]any)
	user := messages[len(messages)-1].(map[string]any)
	content := user["content"].([]any)
	image := content[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("image content was not preserved: %#v", image)
	}
	if len(messages) != 3 {
		t.Fatalf("expected original multimodal system message plus configured system message and user message, got %d", len(messages))
	}
}

func TestAutodiscoveryListsAndRoutesModels(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "provider-secret")
	discoveryRequests := 0
	var routedModel string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/models":
			discoveryRequests++
			return jsonResponse(r, `{"object":"list","data":[{"id":"alpha"},{"id":"beta"}]}`), nil
		case "/v1/chat/completions":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			routedModel, _ = payload["model"].(string)
			return jsonResponse(r, `{"usage":{"total_tokens":1}}`), nil
		default:
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
			return nil, nil
		}
	})

	cfg := testConfig("http://provider.invalid/v1")
	provider := cfg.Providers["test"]
	provider.Autodiscover = config.AutodiscoverConfig{
		Enabled:  true,
		Prefix:   "test/",
		CacheTTL: config.Duration{Duration: time.Minute},
	}
	cfg.Providers["test"] = provider
	g := mustGateway(t, cfg)
	g.client.Transport = transport

	listResponse := httptest.NewRecorder()
	g.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), `"id":"test/alpha"`) {
		t.Fatalf("unexpected model list: status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test/alpha","messages":[]}`))
	response := httptest.NewRecorder()
	g.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected response: status=%d body=%s", response.Code, response.Body.String())
	}
	if routedModel != "alpha" {
		t.Fatalf("expected upstream model alpha, got %q", routedModel)
	}
	if discoveryRequests != 1 {
		t.Fatalf("expected cached model list, got %d discovery requests", discoveryRequests)
	}
}

func TestFailoverUsesNextTarget(t *testing.T) {
	var hosts []string
	var routedModel string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		if r.URL.Host == "first.invalid" {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("unavailable")),
				Request:    r,
			}, nil
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		routedModel, _ = payload["model"].(string)
		return jsonResponse(r, `{"usage":{"total_tokens":2}}`), nil
	})
	cfg := testConfig("http://unused.invalid/v1")
	cfg.Providers = map[string]config.ProviderConfig{
		"first":  {BaseURL: "http://first.invalid/v1"},
		"second": {BaseURL: "http://second.invalid/v1"},
	}
	cfg.Models["stable"] = config.ModelConfig{Targets: []config.TargetConfig{
		{Provider: "first", Model: "model-a"},
		{Provider: "second", Model: "model-b"},
	}}
	g := mustGateway(t, cfg)
	g.client.Transport = transport

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"stable","messages":[]}`))
	response := httptest.NewRecorder()
	g.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	if strings.Join(hosts, ",") != "first.invalid,second.invalid" || routedModel != "model-b" {
		t.Fatalf("failover did not route correctly: hosts=%v model=%q", hosts, routedModel)
	}
}

func TestProviderTimeoutFailsOver(t *testing.T) {
	var hosts []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		if r.URL.Host == "slow.invalid" {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return jsonResponse(r, `{"usage":{"total_tokens":1}}`), nil
	})
	cfg := testConfig("http://unused.invalid/v1")
	cfg.Providers = map[string]config.ProviderConfig{
		"slow":   {BaseURL: "http://slow.invalid/v1", RequestTimeout: config.Duration{Duration: 10 * time.Millisecond}},
		"second": {BaseURL: "http://second.invalid/v1"},
	}
	cfg.Models["stable"] = config.ModelConfig{Targets: []config.TargetConfig{
		{Provider: "slow", Model: "model-a"},
		{Provider: "second", Model: "model-b"},
	}}
	g := mustGateway(t, cfg)
	g.client.Transport = transport

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"stable","messages":[]}`))
	response := httptest.NewRecorder()
	g.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Join(hosts, ",") != "slow.invalid,second.invalid" {
		t.Fatalf("provider timeout did not fail over: status=%d hosts=%v body=%s", response.Code, hosts, response.Body.String())
	}
}

func TestProviderCompatibility(t *testing.T) {
	payload := map[string]any{"thinking": false, "max_tokens": float64(128)}
	applyProviderCompatibility(payload, "/v1/chat/completions", "ollama")
	if payload["reasoning_effort"] != "none" {
		t.Fatalf("ollama thinking override was not translated to reasoning_effort=none: %#v", payload)
	}
	if _, exists := payload["thinking"]; exists {
		t.Fatalf("ollama thinking field was not removed: %#v", payload)
	}
	if _, exists := payload["think"]; exists {
		t.Fatalf("native Ollama think field leaked into /v1 request: %#v", payload)
	}
	withReasoning := map[string]any{"thinking": false, "reasoning_effort": "high"}
	applyProviderCompatibility(withReasoning, "/v1/chat/completions", "ollama")
	if withReasoning["reasoning_effort"] != "high" {
		t.Fatalf("explicit reasoning_effort should win: %#v", withReasoning)
	}
	think := map[string]any{"think": true}
	applyProviderCompatibility(think, "/v1/chat/completions", "ollama")
	if think["reasoning_effort"] != "medium" {
		t.Fatalf("think=true was not translated: %#v", think)
	}
	responses := map[string]any{"max_tokens": float64(128)}
	applyProviderCompatibility(responses, "/v1/responses", "openai")
	if responses["max_output_tokens"] != float64(128) {
		t.Fatalf("responses token override was not translated: %#v", responses)
	}
}

func TestRetryAfterTransportFailure(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "provider-secret")
	attempts := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("temporary transport failure")
		}
		return jsonResponse(r, `{"usage":{"total_tokens":1}}`), nil
	})
	cfg := testConfig("http://provider.invalid/v1")
	model := cfg.Models["stable"]
	model.Retries = 1
	cfg.Models["stable"] = model
	g := mustGateway(t, cfg)
	g.client.Transport = transport

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"stable","messages":[]}`))
	response := httptest.NewRecorder()
	g.ServeHTTP(response, request)
	if response.Code != http.StatusOK || attempts != 2 {
		t.Fatalf("expected successful retry, status=%d attempts=%d body=%s", response.Code, attempts, response.Body.String())
	}
}

func TestLimitStoreRateAndConcurrency(t *testing.T) {
	store := newLimitStore()
	rate := admissionScope{key: "rate", limits: config.Limits{RequestsPerMinute: 60, Burst: 1}}
	release, ok := store.admit(rate)
	if !ok {
		t.Fatal("first request should be admitted")
	}
	release()
	if _, ok := store.admit(rate); ok {
		t.Fatal("second immediate request should be rate limited")
	}

	concurrency := admissionScope{key: "concurrency", limits: config.Limits{MaxConcurrent: 1}}
	release, ok = store.admit(concurrency)
	if !ok {
		t.Fatal("first concurrent request should be admitted")
	}
	if _, ok := store.admit(concurrency); ok {
		t.Fatal("second concurrent request should be rejected")
	}
	release()
	if release, ok = store.admit(concurrency); !ok {
		t.Fatal("request should be admitted after release")
	}
	release()
}

func TestUsagePersistsAcrossStoreRestart(t *testing.T) {
	path := t.TempDir() + "/usage.db"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := newUsageStore(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	store.record("persistent", http.StatusOK, []byte(`{"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`))
	if err := store.close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := newUsageStore(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	usage := reopened.snapshot()["persistent"]
	if usage.Requests != 1 || usage.InputTokens != 4 || usage.OutputTokens != 2 || usage.TotalTokens != 6 {
		t.Fatalf("usage was not persisted: %+v", usage)
	}
}

func jsonResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func testConfig(baseURL string) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			RequestTimeout:  config.Duration{Duration: time.Minute},
			MaxRequestBytes: 1 << 20,
		},
		Providers: map[string]config.ProviderConfig{
			"test": {BaseURL: baseURL, APIKeyEnv: "TEST_PROVIDER_KEY"},
		},
		Models: map[string]config.ModelConfig{
			"stable": {
				Provider:            "test",
				Model:               "provider-model",
				PrependSystemPrompt: "before",
				AppendSystemPrompt:  "after",
			},
		},
	}
}

func mustGateway(t *testing.T, cfg *config.Config) *Gateway {
	t.Helper()
	g, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}
