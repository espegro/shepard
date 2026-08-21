# Running Shepard with systemd

`deployment/install-systemd.sh` installs Shepard as a least-privilege systemd
service on RHEL 9+ compatible distributions and Ubuntu 24.04+.

The installer does not install operating-system packages. Build or obtain the
Shepard binary first, then run it as root (or with `sudo`):

```sh
go build -o shepard ./cmd/shepard
sudo deployment/install-systemd.sh ./shepard ./shepard.example.yaml
```

The second argument is copied only when `/etc/shepard/shepard.yaml` does not
already exist. Existing configuration and credential files are never replaced.

## Configure and start

Review the installed configuration:

```sh
sudoedit /etc/shepard/shepard.yaml
```

Provider credentials are loaded from:

- Ubuntu: `/etc/default/shepard`
- RHEL-compatible systems: `/etc/sysconfig/shepard`

These files are created with mode `0600`. Add only the environment variables
referenced by `api_key_env`, for example:

```sh
OPENAI_API_KEY=replace-me
```

Then validate and start the service:

```sh
sudo -u shepard /usr/local/bin/shepard -config /etc/shepard/shepard.yaml -check
sudo systemctl start shepard
sudo systemctl status shepard
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
```

Enable startup at boot with:

```sh
sudo systemctl enable shepard
```

## Updates and logs

Install a new binary over `/usr/local/bin/shepard`, then restart:

```sh
sudo install -m 0755 ./shepard /usr/local/bin/shepard
sudo systemctl restart shepard
```

View logs with:

```sh
journalctl -u shepard -f
```

Shepard's normal JSON logs include provider attempts, completed requests,
latency, status, the direct `remote_addr`, the effective `client_addr`, and
the raw `x_forwarded_for` value. Request and response bodies are not logged by
default. If temporary body logging is required, enable it explicitly under
`server.logging` and keep `max_body_bytes` small; prompts and generated output
may contain sensitive information.

When running behind a reverse proxy, restrict access with both the proxy and
Shepard's optional network controls:

```yaml
server:
  client_networks:
    - 10.20.0.0/16
  trusted_proxy_networks:
    - 127.0.0.1/32
```

`trusted_proxy_networks` must contain only the proxy/load-balancer networks.
Shepard uses `X-Forwarded-For` for ACL decisions only when the direct TCP peer
matches that list. Otherwise it uses the direct peer address and ignores the
forwarded value for access control.

The service stores its SQLite usage database under `/var/lib/shepard` when the
configuration uses a relative `usage_db` path. The unit restricts writes to
that directory and protects the rest of the filesystem. A restrictive umask
keeps newly created database and WAL files unreadable by other users.

The installed unit also drops all capabilities, hides devices and sensitive
kernel interfaces, restricts namespaces and executable memory, and applies the
`@system-service` system-call allowlist. These restrictions retain the network
and `/var/lib/shepard` access required by Go and SQLite.

## Uninstall

The installer does not provide an uninstall command. To remove the service,
stop and disable it first, then remove the unit and installation files after
reviewing the data you want to keep:

```sh
sudo systemctl disable --now shepard
sudo rm /etc/systemd/system/shepard.service /usr/local/bin/shepard
sudo systemctl daemon-reload
```

Configuration, credentials, and usage data remain under `/etc/shepard` and
`/var/lib/shepard` until removed explicitly.
