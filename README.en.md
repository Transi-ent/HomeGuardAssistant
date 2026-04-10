# HomeGuardAssistant

[中文说明](README.md)

## Project Overview

HomeGuardAssistant is a low-cost home monitoring solution designed for reused devices (currently Linux/Windows laptops with cameras).

It contains two parts:
- Server: Web console, device approval workflow, remote control, scheduling, media management
- Client: photo/video capture, offline buffering, and auto upload after reconnect

## Features

### Server
- Web console login and session management
- Auto device registration request with approval states (`pending` / `approved` / `rejected`)
- Device secret hash storage (no plaintext secret persistence)
- Remote commands: capture photo, start recording, stop recording
- Schedule management: create, toggle, delete
- Media management: preview and delete photos/videos
- Audit API: `/api/console/audit`
- Built-in audit panel in console (latest 50 events)

### Client
- Cross-platform camera capture: Linux (V4L2) and Windows (DirectShow)
- `config.json` based startup, with environment variable overrides
- Auto registration on startup (no manual pre-creation required)
- Uploads are allowed only after server approval
- Offline queue with auto-sync (default quota: 5GB)
- Graceful recording stop for better Windows playback compatibility (H.264 Main@L4.0 + yuv420p + faststart)

## Quick Start

### 1) Start the server

```bash
cd deploy
cp .env.example .env
docker compose up -d --build
```

Open: `http://<server-ip>:38080`  
Default username: `fuyou`  
Default password: `12345678`

### 2) Build the client

Linux / macOS Shell:

```bash
cd client-go
go build -o homeguard-client .
```

Windows PowerShell:

```powershell
cd client-go
go build -o homeguard-client.exe .
```

### 3) Configure and start the client

#### Linux

```bash
cd client-go
chmod +x install.sh
./install.sh http://<server-ip>:38080 ws://<server-ip>:38080/ws/device /dev/video0
```

#### Windows

```powershell
cd client-go
.\install-windows.ps1 -ServerHttp "http://<server-ip>:38080" -ServerWs "ws://<server-ip>:38080/ws/device" -CameraDevice "Integrated Camera"
```

### 4) Approve and verify

1. Start the client; it appears in the console approval list
2. Approve the device in the console
3. After approval, uploads and remote commands become available

## Configuration

Client config example: `client-go/config.example.json`

Important fields:
- `server_http`
- `server_ws`
- `device_id` (auto-generated if empty)
- `device_secret` (auto-generated and persisted if empty)
- `device_name`
- `camera_device`
- `outbox_dir`
- `max_storage_bytes`
- `poll_interval_seconds`

Environment overrides:
- `HG_CONFIG`
- `HG_SERVER_HTTP`
- `HG_SERVER_WS`
- `HG_DEVICE_ID`
- `HG_DEVICE_SECRET`
- `HG_DEVICE_NAME`
- `HG_CAMERA_DEVICE`
- `HG_OUTBOX_DIR`
- `HG_MAX_STORAGE_BYTES`
- `HG_POLL_INTERVAL_SECONDS`

## FAQ

### How to list camera device names on Windows?

```powershell
ffmpeg -list_devices true -f dshow -i dummy
```

Use the listed camera name in `camera_device`.
