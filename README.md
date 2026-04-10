# HomeGuardAssistant（看家助理）

[English README](README.en.md)

## 项目简介

HomeGuardAssistant 是一个低成本看家护院方案，适用于利旧设备（当前重点为 Linux / Windows 笔记本摄像头）。
系统由服务端与客户端组成：
- 服务端：提供 Web 控制台、设备审批、远程指令、定时任务、媒体管理
- 客户端：负责拍照/录像采集、离线缓存、联网后补传

## 功能特性

### 服务端
- Web 控制台登录与会话管理
- 设备自动注册申请（`pending`）与审批流（`approved` / `rejected`）
- 设备密钥哈希存储（避免明文保存）
- 设备远程指令：拍照、开始录像、停止录像
- 定时任务：创建、启停、删除
- 媒体管理：图片/视频预览与删除
- 审计日志接口：`/api/console/audit`
- 控制台审计面板：最近 50 条记录可刷新查看

### 客户端
- Linux（V4L2）与 Windows（DirectShow）双平台采集
- `config.json` 配置启动，支持环境变量覆盖
- 首次启动自动注册，无需先手工建设备
- 审批通过后才允许上传
- 离线缓存与自动补传（默认上限 5GB）
- 录像优雅结束，提升 Windows 播放兼容性（H.264 Main@L4.0 + yuv420p + faststart）

## 快速开始

### 1) 启动服务端

```bash
cd deploy
cp .env.example .env
docker compose up -d --build
```

访问：`http://<server-ip>:38080`  
默认账号：`admin`  
默认密码：`12345678`

### 2) 构建客户端

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

### 3) 配置并启动客户端

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

### 4) 审批并验证

1. 客户端启动后，设备会进入控制台“设备审批”列表
2. 管理员点击“同意”
3. 审批通过后即可上传媒体并接收远程指令

## 配置说明

客户端配置示例见：`client-go/config.example.json`

关键字段：
- `server_http`：服务端 HTTP 地址
- `server_ws`：服务端设备 WS 地址
- `device_id`：设备 ID（为空自动生成）
- `device_secret`：设备密钥（为空自动生成并持久化）
- `device_name`：设备显示名
- `camera_device`：摄像头设备名/路径
- `outbox_dir`：离线缓存目录
- `max_storage_bytes`：缓存上限（默认 5GB）
- `poll_interval_seconds`：审批轮询间隔

环境变量覆盖：
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

## 常见问题

### Windows 如何查看摄像头设备名？

```powershell
ffmpeg -list_devices true -f dshow -i dummy
```

将输出中的设备名写入 `camera_device`。
