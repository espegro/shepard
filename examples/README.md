# Example configurations

Choose the smallest configuration that matches your deployment:

- [`minimal.yaml`](minimal.yaml) — one OpenAI-compatible provider and one stable alias
- [`local-autodiscovery.yaml`](local-autodiscovery.yaml) — a local backend with discovered models
- [`production-failover.yaml`](production-failover.yaml) — authentication, limits, queueing, and provider failover

Copy an example before editing it:

```sh
cp examples/minimal.yaml shepard.yaml
go run ./cmd/shepard -config shepard.yaml -check
```

Provider credentials are read from environment variables named by `api_key_env`.
Do not put provider secrets directly in YAML. When inbound authentication is
enabled, `server.inbound_api_keys` contains the client-facing bearer keys and
should be protected accordingly.

For deployments behind a reverse proxy, see the comments in
`production-failover.yaml` for `client_networks` and
`trusted_proxy_networks`. Do not trust `X-Forwarded-For` unless the proxy
network is explicitly listed. Request/response body logging is disabled by
default and should only be enabled temporarily for debugging.
