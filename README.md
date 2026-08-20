# Shepard

Shepard is a small, self-hosted OpenAI-compatible gateway for routing LLM
requests to one or more backends. It gives clients one stable endpoint and
stable model names while the operator keeps provider credentials, backend
model IDs, failover policy, and capacity controls on the server.

## What Shepard solves

Running several LLM backends normally forces every client to know provider-
specific URLs, model names, credentials, and failure behavior. Shepard puts
that operational complexity behind one API:

- Clients call a stable alias such as `coding_model` instead of a vendor model ID.
- Providers can be changed or reordered without changing client configuration.
- Credentials stay in the gateway environment instead of being sent by clients.
- Requests can fail over from a local backend to a remote provider.
- OpenCode and other OpenAI-compatible clients can use the same endpoint.
- Operators get authentication, network ACLs, limits, queueing, readiness, metrics,
  usage accounting, and structured logs in one small service.

## Features

- OpenAI-compatible `chat/completions` and `responses` APIs, including streaming
- Stable model aliases with optional system-prompt prepend/append
- Multiple providers with ordered targets, retries, and failover
- Backend model autodiscovery through `/v1/models`, with collision-safe prefixes
- Provider API keys from environment variables and custom upstream headers
- Per-client, per-model, and per-provider request-rate and concurrency limits
- Small bounded queue for short overload bursts, with timeout-aware rejection
- Optional IPv4/IPv6 client ACLs using single addresses or CIDR ranges
- Optional trusted-proxy handling for `X-Forwarded-For`
- Separate liveness (`/healthz`) and provider readiness (`/readyz`) endpoints
- Prometheus-compatible metrics and SQLite-backed usage totals
- Optional redacted request/response logging with bounded body capture
- Generated OpenCode configuration at `/opencode.json`
- Atomic configuration reload with `SIGHUP`
  - systemd installer and operating documentation for RHEL 9+ and Ubuntu 24.04+

The main design decisions and trade-offs are documented in
[docs/design.md](docs/design.md).

## Request flow

```text
Client
  -> auth and network ACL
  -> stable model alias
  -> client/model limits and bounded queue
  -> provider target selection, retry, and failover
  -> OpenAI-compatible backend
```

Shepard forwards the request body, rewrites only the configured model alias,
adds configured system-prompt text when requested, and streams the selected
provider response back to the client. It does not permanently disable a
backend based on a single transient failure; failover is request-scoped.

## Image inputs

OpenAI-compatible multimodal requests are passed through to providers without
rewriting their content blocks. A client can send an image as a remote URL or
as a base64 data URL:

```json
{
  "model": "vision_model",
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "Describe this image."},
        {
          "type": "image_url",
          "image_url": {"url": "data:image/jpeg;base64,..."}
        }
      ]
    }
  ]
}
```

The selected backend must support vision and the request must fit within
`server.max_request_bytes` (8 MiB by default). Increase that limit for larger
base64 images. Shepard does not download or inspect remote image URLs. Image
capability is provider/model-specific; autodiscovery exposes the model, but it
does not guarantee that every model accepts images.

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

## Multiple backends and credentials

Each entry under `providers` is an independent upstream configuration. A
provider has its own base URL, credential environment variable, headers,
autodiscovery settings, and rate/concurrency limits:

```yaml
providers:
  openai:
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY

  local:
    base_url: http://127.0.0.1:11434/v1
    protocol: ollama
    request_timeout: 5m
    api_key_env: LOCAL_API_KEY

  backup:
    base_url: https://backup.example.com/v1
    api_key_env: BACKUP_API_KEY
```

The environment variables contain the provider credentials. They are read by
Shepard and sent only to the selected upstream provider; client credentials are
never used as provider credentials.

`protocol` defaults to `openai`. Set it to `ollama` for Ollama-compatible
providers so Shepard can translate fields such as `thinking` to Ollama's
`think` option. `request_timeout` is an optional per-provider deadline; when a
provider exceeds it, Shepard can fail over to the next target instead of
waiting for the global request timeout.

A model can target one provider:

```yaml
models:
  fast:
    provider: local
    model: qwen-coder
```

Or it can define an ordered list of targets for retries and failover:

```yaml
models:
  coding:
    retries: 1
    targets:
      - provider: local
        model: qwen-coder
      - provider: openai
        model: gpt-5-mini
      - provider: backup
        model: backup-coding-model
```

Shepard retries transient transport and server failures on the current target,
then advances through the remaining targets when failover is possible. A
`404` or `429` moves directly to the next target. Clients continue to request
the stable alias `coding`; they do not need to know which backend handled the
request.

The same backend can be configured more than once when separate API keys or
limits are needed:

```yaml
providers:
  account_a:
    base_url: https://api.example.com/v1
    api_key_env: ACCOUNT_A_KEY

  account_b:
    base_url: https://api.example.com/v1
    api_key_env: ACCOUNT_B_KEY

models:
  shared-service:
    targets:
      - provider: account_a
        model: shared-model
      - provider: account_b
        model: shared-model
```

This provides explicit ordered failover between keys. Shepard does not yet
perform automatic least-loaded key pooling or key rotation within one provider.

## Model request overrides

Models can apply server-side request fields consistently, regardless of which
client or target handles the request:

```yaml
models:
  coding:
    provider: openai
    model: gpt-5-mini
    overrides:
      temperature: 0.2
      top_p: 0.9
      max_output_tokens: 8192
      reasoning:
        effort: medium
```

Overrides are applied after the client request and therefore take precedence.
This is useful for keeping coding models deterministic, setting output limits,
or enabling a provider's reasoning/thinking mode. Common fields such as
`temperature`, `top_p`, `max_tokens`, `max_output_tokens`, `stop`, `seed`,
`frequency_penalty`, `presence_penalty`, `reasoning`, and `thinking` can be
passed through. Shepard does not translate provider-specific reasoning fields;
the configured shape is sent to the selected backend.

Structural fields such as `model`, `messages`, `input`, and `instructions` are
protected and cannot be replaced through `overrides`. Client fields not listed
in `overrides` continue to pass through unchanged.

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
