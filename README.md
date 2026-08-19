# Shepard

Shepard is a small OpenAI-compatible LLM gateway written in Go. Clients use stable
model aliases while Shepard selects the provider, supplies its credential, rewrites
the upstream model name, and optionally extends the system prompt.

## Quick start

```sh
cp shepard.example.yaml shepard.yaml
export OPENAI_API_KEY='...'
go run ./cmd/shepard -config shepard.yaml
```

The gateway listens without authentication when `inbound_api_keys` is empty:

```sh
curl http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"smart","messages":[{"role":"user","content":"Hello"}]}'
```

Set one or more `server.inbound_api_keys` to require `Authorization: Bearer ...`
from clients. Provider credentials are read from the environment variable named by
`api_key_env`; secrets therefore do not need to be stored in YAML.

An optional network allowlist can restrict which client IP addresses may reach
the listener. It accepts single IPv4/IPv6 addresses and CIDR ranges:

```yaml
server:
  client_networks:
    - 127.0.0.1
    - 192.168.10.0/24
    - ::1
    - 2001:db8:1234::/48
```

When the list is non-empty, only matching TCP peer addresses are accepted;
other requests receive `403`. Shepard does not trust `X-Forwarded-For`
automatically. If Shepard is behind a reverse proxy, explicitly configure the
proxy network before using forwarded client addresses:

```yaml
server:
  trusted_proxy_networks:
    - 127.0.0.1/32
    - 10.0.0.0/8
```

Only requests whose direct TCP peer matches a trusted proxy network use
`X-Forwarded-For`; otherwise the direct peer address is used. Basic request
completion logs include `remote_addr`, the effective `client_addr`, and the
raw `x_forwarded_for` value. Enforce the same policy at the proxy/firewall
layer as defense in depth.

## Routes

- `POST /v1/chat/completions` (including streaming)
- `POST /v1/responses` (including streaming)
- `GET /v1/models` lists static aliases and prefixed discovered models
- `GET /opencode.json` returns an OpenCode-compatible config for this gateway
- `GET /_shepard/usage` reports process-local token totals returned by providers
- `GET /_shepard/metrics` exposes Prometheus-compatible process metrics
- `GET /healthz` is intentionally unauthenticated
- `GET /readyz` checks whether at least one provider is reachable

All routes except `/healthz` and `/readyz` follow the configured inbound
authentication policy.
Token totals only include providers that return a `usage` object. For streamed
OpenAI chat requests, request usage from the provider with
`stream_options: {"include_usage": true}`.

`/opencode.json` builds its base URL from the request host (and
`X-Forwarded-Proto` when present), and includes configured static and discovered
model aliases. It never exports API keys; when inbound authentication is
enabled, configure the corresponding Shepard key in OpenCode separately.

Usage is stored in the SQLite file configured by `server.usage_db` and survives
restarts. The database path requires a process restart if changed; other settings
can still be reloaded with `SIGHUP`.

## Backend model discovery

An OpenAI-compatible backend can publish its models through `GET /v1/models`:

```yaml
providers:
  local:
    base_url: http://127.0.0.1:10000/v1
    autodiscover:
      enabled: true
      prefix: "local/"
      cache_ttl: 30s
```

If the backend returns a model named `llama`, clients see and request it as
`local/llama`. Prefixes prevent collisions between providers. Discovered model
lists are cached for `cache_ttl`; static aliases take priority when names overlap.
Discovery uses the provider's configured API key and custom headers.

A stable meta-model can point to one of the backend models independently of
autodiscovery:

```yaml
models:
  coding_model:
    retries: 1
    targets:
      - provider: local
        model: qwen3.8:27b
      - provider: openai
        model: gpt-5-mini
```

Clients request `coding_model`. Shepard retries transient transport and `5xx`
failures, then moves through `targets` in order. A `404` or `429` moves directly to
the next target. Non-retryable provider responses are returned to the client. Failover is
only possible before response headers are sent; an interrupted stream is not
replayed midway through its output.

The original single-target syntax remains valid:

```yaml
models:
  coding_model:
    provider: local
    model: qwen3.8:27b
```

## Request and concurrency limits

Limits use the same shape at client, model, and provider level:

```yaml
server:
  client_limits:
    requests_per_minute: 120
    burst: 10
    max_concurrent: 4

providers:
  local:
    base_url: http://127.0.0.1:10000/v1
    limits:
      requests_per_minute: 60
      burst: 5
      max_concurrent: 2

models:
  coding_model:
    provider: local
    model: qwen3.8:27b
    limits:
      requests_per_minute: 30
      burst: 3
      max_concurrent: 1
```

Client limits apply per inbound API key, or per source IP when authentication is
disabled. Model and provider limits are shared across clients. A zero or omitted
value disables that limit. Rejected requests return `429` and `Retry-After: 1`.

## Optional request logging

Request and provider-response logging is disabled by default. Enable it only
when needed for debugging:

```yaml
server:
  logging:
    enabled: true
    requests: true
    responses: true
    include_bodies: true
    max_body_bytes: 65536
```

Headers containing authorization, cookies, or API keys are redacted. Bodies
are truncated at `max_body_bytes`, but may still contain prompts, generated
content, or other sensitive data. Prefer enabling this temporarily and review
the JSON logs before sharing them.

When concurrency or rate limits are temporarily exhausted, Shepard places a
small bounded number of requests in a queue (32 requests and 30 seconds by
default). Set `server.queue.enabled: false` to disable it, or tune
`server.queue.max_size` and `server.queue.wait_timeout`. Requests that cannot
enter or leave the queue within the timeout still receive `429`.

For local testing, [shepard.test.yaml](shepard.test.yaml) points to an
unauthenticated backend on `127.0.0.1:10000` and enables discovery.

## Stable meta-models and reload

The keys below `models` are the names exposed to clients. Change `provider` or
`model` behind an alias, validate the file, then reload without dropping active
connections:

```sh
go run ./cmd/shepard -config shepard.yaml -check
kill -HUP "$(pidof shepard)"
```

Invalid reloads are rejected and the previous configuration remains active.

## systemd deployment

For RHEL 9+ compatible systems and Ubuntu 24.04+, see
[docs/systemd.md](docs/systemd.md) and run
`sudo deployment/install-systemd.sh` after building the binary.

## Example configurations

Several readable starting points are available in [examples/](examples/):

- [minimal.yaml](examples/minimal.yaml) for one local or remote provider
- [local-autodiscovery.yaml](examples/local-autodiscovery.yaml) for a local backend
- [production-failover.yaml](examples/production-failover.yaml) for authentication, limits, and failover

## Roadmap

The queue currently uses fixed bounds. A future capacity-aware queue may learn
provider/model throughput and estimate whether a request can complete before
its timeout. See [status.md](status.md) for the deferred design and current
project status.
