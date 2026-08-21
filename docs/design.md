# Shepard design notes

This document records the main design choices behind Shepard. The README
describes how to run the service; this document explains why the service is
structured this way.

## Goals and scope

Shepard is a small, self-hosted routing layer for OpenAI-compatible LLM
backends. Its primary goals are:

- Give clients one stable API endpoint and stable model aliases.
- Keep provider credentials and backend model IDs server-side.
- Make provider changes, retries, and failover configuration-driven.
- Provide useful operational controls without requiring a large platform.
- Preserve streaming behavior and avoid replaying partial responses.

Shepard is not intended to be a general API management platform, a token
accounting authority, or a replacement for a network firewall and TLS proxy.

## High-level request flow

```text
HTTP client
    |
    v
Network ACL -> authentication -> early ingress/client admission
    |
    v
Request size/timeout -> model alias resolution -> model admission and bounded queue
    |
    v
Ordered provider targets -> provider admission -> upstream request
    |                                |
    |<------ retry / failover --------|
    v
Stream selected upstream response to client
```

The request body is decoded as JSON so Shepard can resolve the model alias and
apply configured prompt or request overrides. The resulting request is then
encoded and sent to the selected provider.

## Configuration model

The configuration has three deliberately separate concepts:

- `providers` describe upstream endpoints, credentials, headers, discovery,
  and provider-wide limits.
- `models` describe client-facing aliases, target order, retries, prompts,
  request overrides, and model-wide limits.
- `server` describes listener behavior, authentication, queueing, logging,
  readiness, and client-wide limits.

This separation means a client can keep requesting `coding` while the operator
changes its provider or upstream model.

## Providers, credentials, and failover

Each provider has its own `base_url` and optional `api_key_env`. Shepard reads
the named environment variable and applies the resulting credential only to
requests sent to that provider. The inbound client `Authorization` header is
never used as an upstream provider credential.

Targets are ordered. Shepard retries transient transport and server failures
according to the model's retry count, then advances to the next target when
possible. A `404` or `429` advances directly to the next target. Once response
headers have been sent, Shepard cannot safely replay an interrupted stream, so
failover is only performed before streaming starts.

The same URL can appear as multiple providers when different API keys,
headers, or limits are required. Automatic least-loaded key pooling is not
currently implemented; explicit provider entries and target order are used
instead.

Providers may declare a protocol (`openai` by default or `ollama`) and an
optional per-provider request timeout. Protocol adapters make small
provider-specific field translations, such as Ollama's `think` option. A
provider timeout bounds an individual attempt and allows ordered failover when
one backend is slow; the global server timeout still bounds the whole request.

## Model discovery and aliases

Autodiscovery calls a provider's `/v1/models` endpoint and caches the result for
the configured TTL. Discovered names receive a provider-specific prefix, such
as `local/llama`, to prevent collisions. Static aliases take priority when a
name overlaps a discovered name.

Discovery is intentionally lazy. A provider being unavailable should not stop
Shepard from starting, and static aliases can still be served while an
autodiscovery provider is down.

## Request overrides and prompts

Model-level `overrides` are applied after client fields, so configured policy
wins over client-provided values. Structural fields (`model`, `messages`,
`input`, and `instructions`) are protected. Provider-specific fields such as
`reasoning` or `thinking` are passed through where possible, with protocol
adapters normalizing known differences. In particular, Ollama's OpenAI-
compatible `/v1` endpoint uses `reasoning_effort` (where `none` disables
thinking), while its native `/api` endpoint uses `think`.

Prompt prepend/append behavior is implemented separately because it must handle
Chat Completions messages and Responses instructions differently.

## Limits and queueing

Admission limits exist at global ingress, client, model, and provider scope.
Global ingress and client admission happen before the request body is read;
model and provider admission happen after routing information is known. The
limiter uses token buckets for request rate and counters for active
concurrency. A rejected admission returns `429` rather than creating an
unbounded in-memory queue.

The bounded queue is a small backpressure buffer for short overload bursts. It
has a configurable maximum size and wait timeout. Requests that cannot be
admitted within that window receive `429`. The queue is intentionally fixed and
conservative today; it does not claim to know the backend's true token
throughput.

## Authentication and network ACLs

Inbound bearer keys protect the gateway API. An empty key list means
authentication is disabled, which is useful for localhost but unsafe on a
shared network.

`client_networks` is an optional IPv4/IPv6 allowlist. It accepts individual
addresses and CIDR ranges and is evaluated before authentication and request
body processing. It is defense in depth, not a replacement for a firewall.

The direct TCP peer address is trusted by default. `X-Forwarded-For` is used for
ACL decisions only when the direct peer matches an explicitly configured
`trusted_proxy_networks` entry. This prevents arbitrary clients from spoofing
their source address through a forwarded header.

## Logging and sensitive data

Normal structured logs contain lifecycle events, provider attempts, request
completion, latency, status, and network address fields. Full request/response
logging is disabled by default. When enabled, authorization, cookie, and API
key headers are redacted and bodies are bounded by a configured byte limit.

Body logging remains sensitive because prompts and generated content can be
confidential. It is intended for short-lived debugging, not routine operation.

## Health, readiness, and metrics

- `/healthz` answers whether the Shepard process is alive.
- `/readyz` probes providers and returns `503` when none are reachable. Probes
  are coalesced and briefly cached to prevent request fan-out amplification.
- `/_shepard/metrics` exposes process and queue counters in Prometheus format.
- `/_shepard/usage` reports persisted token totals when providers return usage.

Health and readiness are separate so an orchestrator can distinguish a live
process from a process that currently has no usable backend.

## Configuration reload and lifecycle

`SIGHUP` parses and validates a new configuration before swapping it into the
gateway. Invalid configurations leave the previous configuration active.
Usage database location is fixed for the process because changing it safely
would require reopening the store; a restart is required for that setting.

Active HTTP requests continue during a successful reload. Shutdown uses the
HTTP server's graceful shutdown path and gives in-flight requests a bounded
window to finish.

## Deferred design work

The following are intentionally not part of the current design:

- Capacity-aware queueing based on learned provider/model throughput
- Provider circuit breakers and cooldown state
- Automatic API-key pooling or least-loaded key selection
- Full per-provider/model latency histograms
- Native TLS termination and certificate management

The current implementation favors predictable behavior, bounded resources, and
explicit configuration over opaque adaptive behavior.
