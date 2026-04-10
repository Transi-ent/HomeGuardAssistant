#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "Usage: $0 <server_http> <server_ws> [camera_device]"
  echo "Example: $0 http://192.168.1.2:38080 ws://192.168.1.2:38080/ws/device /dev/video0"
  exit 1
fi

SERVER_HTTP="$1"
SERVER_WS="$2"
CAMERA_DEVICE="${3:-/dev/video0}"

BIN_DIR="${HOME}/.local/bin"
CFG_DIR="${HOME}/.config/homeguard"
OUTBOX_DIR="${HOME}/.local/share/homeguard/outbox"
SERVICE_DIR="${HOME}/.config/systemd/user"

mkdir -p "${BIN_DIR}" "${CFG_DIR}" "${OUTBOX_DIR}" "${SERVICE_DIR}"
cp ./homeguard-client "${BIN_DIR}/homeguard-client"
chmod +x "${BIN_DIR}/homeguard-client"

cat > "${CFG_DIR}/config.json" <<EOF
{
  "server_http": "${SERVER_HTTP}",
  "server_ws": "${SERVER_WS}",
  "device_id": "",
  "device_secret": "",
  "device_name": "$(hostname)",
  "outbox_dir": "${OUTBOX_DIR}",
  "camera_device": "${CAMERA_DEVICE}",
  "max_storage_bytes": 5368709120,
  "poll_interval_seconds": 10
}
EOF

cat > "${SERVICE_DIR}/homeguard-client.service" <<EOF
[Unit]
Description=HomeGuard Client
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=HG_CONFIG=%h/.config/homeguard/config.json
ExecStart=%h/.local/bin/homeguard-client
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now homeguard-client
echo "Installed. Check status: systemctl --user status homeguard-client"
