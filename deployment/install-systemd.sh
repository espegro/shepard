#!/usr/bin/env bash
set -euo pipefail

# Install Shepard as a systemd service on RHEL 9+ or Ubuntu 24.04+.
# Usage: sudo deployment/install-systemd.sh [binary] [config]

if [[ "${EUID}" -ne 0 ]]; then
  echo "This installer must be run as root (use sudo)." >&2
  exit 1
fi

binary_source="${1:-./shepard}"
config_source="${2:-./shepard.example.yaml}"
if [[ ! -f "${binary_source}" ]]; then
  echo "Binary not found: ${binary_source}" >&2
  exit 1
fi
if [[ ! -f "${config_source}" ]]; then
  echo "Config not found: ${config_source}" >&2
  exit 1
fi

# shellcheck disable=SC1091
source /etc/os-release
os_id="${ID:-}"
os_major="${VERSION_ID%%.*}"
case "${os_id}" in
  ubuntu)
    if [[ "${os_major}" -lt 24 ]]; then
      echo "Ubuntu 24.04 or newer is required (found ${VERSION_ID})." >&2
      exit 1
    fi
    env_file=/etc/default/shepard
    ;;
  rhel|rocky|almalinux|centos)
    if [[ "${os_major}" -lt 9 ]]; then
      echo "RHEL 9 or a compatible release is required (found ${PRETTY_NAME})." >&2
      exit 1
    fi
    env_file=/etc/sysconfig/shepard
    ;;
  *)
    echo "Unsupported operating system: ${PRETTY_NAME:-${os_id}}" >&2
    echo "Supported systems: RHEL 9+ and Ubuntu 24.04+." >&2
    exit 1
    ;;
esac

install -d -m 0755 /usr/local/bin /etc/shepard /var/lib/shepard
if ! getent group shepard >/dev/null 2>&1; then
  groupadd --system shepard
fi
if ! id shepard >/dev/null 2>&1; then
  useradd --system --gid shepard --home-dir /var/lib/shepard --no-create-home \
    --shell /usr/sbin/nologin shepard
fi

install -m 0755 "${binary_source}" /usr/local/bin/shepard
if [[ ! -e /etc/shepard/shepard.yaml ]]; then
  install -m 0640 -o root -g shepard "${config_source}" /etc/shepard/shepard.yaml
  echo "Installed initial config at /etc/shepard/shepard.yaml; review it before starting Shepard."
else
  echo "Keeping existing config at /etc/shepard/shepard.yaml."
fi

if [[ ! -e "${env_file}" ]]; then
  printf '%s\n' '# Add provider credentials here, for example:' \
    '# OPENAI_API_KEY=replace-me' > "${env_file}"
  chmod 0600 "${env_file}"
  chown root:root "${env_file}"
  echo "Created ${env_file}; add provider credentials before starting Shepard."
else
  echo "Keeping existing environment file at ${env_file}."
fi

cat > /etc/systemd/system/shepard.service <<EOF
[Unit]
Description=Shepard OpenAI-compatible LLM gateway
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=shepard
Group=shepard
WorkingDirectory=/var/lib/shepard
EnvironmentFile=-${env_file}
ExecStart=/usr/local/bin/shepard -config /etc/shepard/shepard.yaml
Restart=on-failure
RestartSec=5s
StateDirectory=shepard

NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=/var/lib/shepard
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=multi-user.target
EOF

chown -R shepard:shepard /var/lib/shepard
systemctl daemon-reload
systemctl enable shepard.service

echo "Shepard systemd service installed."
echo "1. Review /etc/shepard/shepard.yaml"
echo "2. Add provider credentials to ${env_file}"
echo "3. Run: systemctl start shepard"
echo "4. Check: systemctl status shepard"
