# Shepard project status

## Current state

Shepard is a working OpenAI-compatible LLM gateway with:

- Static model aliases and backend model autodiscovery
- Multiple provider targets with retry and failover
- Per-client, per-model, and per-provider rate/concurrency limits
- A bounded request queue with configurable wait timeout
- `/readyz` provider readiness checks
- Prometheus-compatible metrics at `/_shepard/metrics`
- Optional, redacted request/response logging with bounded body capture
- Optional IPv4/IPv6 client network allowlist with single-address and CIDR support
- Optional trusted-proxy handling for `X-Forwarded-For` with basic network-aware request logs
- Model-level request overrides for generation and reasoning parameters
- OpenCode configuration generation at `/opencode.json`
- SQLite-backed usage accounting
- Configuration reload through `SIGHUP`
- Request and upstream timeouts, including a bounded request-body read window
- systemd installation documentation and an installer for RHEL 9+ and Ubuntu 24.04+
- Readable example configurations for minimal, local autodiscovery, and production failover deployments
- Architecture and design rationale in [`docs/design.md`](docs/design.md)

The test suite passes with the race detector:

```text
go test -race ./...
```

## Deferred work

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
