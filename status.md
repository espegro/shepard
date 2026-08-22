# Shepard project status

## Current state

Shepard is a working OpenAI-compatible LLM gateway with:

- Static model aliases and backend model autodiscovery
- Multiple provider targets with retry and failover
- Early global ingress admission plus per-client, per-model, and per-provider
  rate/concurrency limits
- A bounded request queue with configurable wait timeout
- Coalesced, briefly cached `/readyz` provider readiness checks
- Prometheus-compatible metrics at `/_shepard/metrics`
- Optional, redacted request/response logging with bounded body capture
- Optional IPv4/IPv6 client network allowlist with single-address and CIDR support
- Optional trusted-proxy handling for `X-Forwarded-For` with basic network-aware request logs
- Model-level request overrides for generation and reasoning parameters
- Provider protocol compatibility for Ollama fields and per-provider attempt timeouts
- OpenCode configuration generation at `/opencode.json`
- Pass-through support for OpenAI-compatible multimodal image inputs
- SQLite-backed cumulative and UTC daily/monthly usage accounting per
  pseudonymous client and model
- Configuration reload through `SIGHUP`
- Request and upstream timeouts, including a bounded request-body read window
  and streaming write-idle deadline
- Explicit safe-header allowlists at the client/provider boundary
- systemd installation documentation and an installer for RHEL 9+ and Ubuntu 24.04+
- Readable example configurations for minimal, local autodiscovery, and production failover deployments
- Architecture and design rationale in [`docs/design.md`](docs/design.md)

The test suite passes with the race detector:

```text
go test -race ./...
```

## Deferred work

### Security hardening TODO

High priority:

- Build and deploy Shepard with Go 1.26.6 or newer. The current deployed
  binary was built with Go 1.26.0, and `govulncheck` reports reachable
  standard-library vulnerabilities in HTTP, TLS, URL parsing, and related
  code.
- Require inbound bearer authentication for deployments that are not limited
  to a fully trusted network. Document TLS termination or mTLS through a
  reverse proxy for protecting API keys, prompts, and responses in transit.

Medium priority:

- Reject empty, whitespace-only, duplicate, placeholder, and unreasonably
  short `inbound_api_keys` during configuration validation.
- Expand or make configurable the sensitive-header redaction list used by
  optional request/response logging.
- Consider separating management endpoints (`/readyz`, metrics, and usage)
  onto a dedicated listener or applying separate ACL/authentication policy.
- Validate `Host` and trust `X-Forwarded-Proto` only from configured trusted
  proxies when generating `/opencode.json`.

### Dynamic capacity-aware queue

The current queue uses fixed `max_size` and `wait_timeout` limits. A future
implementation may estimate whether a queued request can finish before its
deadline by learning capacity per provider/model.

Possible signals include:

- Output tokens per second
- Time to first token
- End-to-end request duration
- Observed concurrency and error rate
- Input and output token counts when the backend reports usage

The design should use conservative estimates until enough observations exist,
retain a hard queue/memory limit, and reject requests whose estimated wait plus
service time exceeds the request timeout. This is intentionally deferred; the
current bounded queue remains the safety mechanism.

### Other deferred items

- Provider circuit breaker and cooldown state
- More detailed per-provider/model metrics and latency histograms
- Optional startup backend probes with configurable warning/fail behavior
