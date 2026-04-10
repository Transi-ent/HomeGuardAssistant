package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Config struct {
	ServerHTTP      string `json:"server_http"`
	ServerWS        string `json:"server_ws"`
	DeviceID        string `json:"device_id"`
	DeviceSecret    string `json:"device_secret"`
	DeviceName      string `json:"device_name"`
	OutboxDir       string `json:"outbox_dir"`
	CameraDevice    string `json:"camera_device"`
	MaxStorageBytes int64  `json:"max_storage_bytes"`
	PollIntervalSec int64  `json:"poll_interval_seconds"`
}

type Schedule struct {
	ID              string `json:"id"`
	DeviceID        string `json:"device_id"`
	Type            string `json:"type"`
	IntervalSeconds int64  `json:"interval_seconds"`
	DurationSeconds int64  `json:"duration_seconds"`
	Enabled         bool   `json:"enabled"`
}

type recordSession struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	path  string
}

type Client struct {
	cfgPath string
	cfg     Config

	mu            sync.Mutex
	recordSession *recordSession
	schedulers    map[string]context.CancelFunc
}

func main() {
	cfgPath := env("HG_CONFIG", "./config.json")
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	applyEnvOverrides(&cfg)
	fillConfigDefaults(&cfg)
	if err := ensureClientIdentity(&cfg); err != nil {
		log.Fatal(err)
	}
	if err := saveConfig(cfgPath, cfg); err != nil {
		log.Printf("warn: failed to persist config: %v", err)
	}
	if err := os.MkdirAll(cfg.OutboxDir, 0o755); err != nil {
		log.Fatal(err)
	}

	c := &Client{
		cfgPath:    cfgPath,
		cfg:        cfg,
		schedulers: map[string]context.CancelFunc{},
	}

	for {
		approved, err := c.registerDevice()
		if err != nil {
			log.Printf("register failed: %v", err)
			time.Sleep(time.Duration(c.cfg.PollIntervalSec) * time.Second)
			continue
		}
		if !approved {
			log.Printf("device is pending/rejected, waiting for approval")
			time.Sleep(time.Duration(c.cfg.PollIntervalSec) * time.Second)
			continue
		}

		go c.uploadLoop()
		if err := c.runWS(); err != nil {
			log.Printf("ws disconnected: %v", err)
		}
		time.Sleep(3 * time.Second)
	}
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func saveConfig(path string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func fillConfigDefaults(cfg *Config) {
	defaultCamera := "/dev/video0"
	if runtime.GOOS == "windows" {
		defaultCamera = "Integrated Camera"
	}
	if strings.TrimSpace(cfg.ServerHTTP) == "" {
		cfg.ServerHTTP = "http://127.0.0.1:38080"
	}
	if strings.TrimSpace(cfg.ServerWS) == "" {
		cfg.ServerWS = "ws://127.0.0.1:38080/ws/device"
	}
	if strings.TrimSpace(cfg.OutboxDir) == "" {
		cfg.OutboxDir = "./outbox"
	}
	if strings.TrimSpace(cfg.CameraDevice) == "" {
		cfg.CameraDevice = defaultCamera
	}
	if cfg.MaxStorageBytes <= 0 {
		cfg.MaxStorageBytes = 5 * 1024 * 1024 * 1024
	}
	if cfg.PollIntervalSec <= 0 {
		cfg.PollIntervalSec = 10
	}
	if strings.TrimSpace(cfg.DeviceName) == "" {
		host, _ := os.Hostname()
		cfg.DeviceName = host
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("HG_SERVER_HTTP")); v != "" {
		cfg.ServerHTTP = v
	}
	if v := strings.TrimSpace(os.Getenv("HG_SERVER_WS")); v != "" {
		cfg.ServerWS = v
	}
	if v := strings.TrimSpace(os.Getenv("HG_DEVICE_ID")); v != "" {
		cfg.DeviceID = v
	}
	if v := strings.TrimSpace(os.Getenv("HG_DEVICE_SECRET")); v != "" {
		cfg.DeviceSecret = v
	}
	if v := strings.TrimSpace(os.Getenv("HG_DEVICE_NAME")); v != "" {
		cfg.DeviceName = v
	}
	if v := strings.TrimSpace(os.Getenv("HG_OUTBOX_DIR")); v != "" {
		cfg.OutboxDir = v
	}
	if v := strings.TrimSpace(os.Getenv("HG_CAMERA_DEVICE")); v != "" {
		cfg.CameraDevice = v
	}
	if v := strings.TrimSpace(os.Getenv("HG_MAX_STORAGE_BYTES")); v != "" {
		if n, err := parseInt64(v); err == nil {
			cfg.MaxStorageBytes = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("HG_POLL_INTERVAL_SECONDS")); v != "" {
		if n, err := parseInt64(v); err == nil {
			cfg.PollIntervalSec = n
		}
	}
}

func ensureClientIdentity(cfg *Config) error {
	if strings.TrimSpace(cfg.DeviceID) == "" {
		host, _ := os.Hostname()
		cfg.DeviceID = fmt.Sprintf("%s-%s", sanitize(host), shortRandom())
	}
	if strings.TrimSpace(cfg.DeviceSecret) == "" {
		cfg.DeviceSecret = randomHex(24)
	}
	return nil
}

func (c *Client) registerDevice() (bool, error) {
	body := map[string]any{
		"device_id":     c.cfg.DeviceID,
		"device_secret": c.cfg.DeviceSecret,
		"device_name":   c.cfg.DeviceName,
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, c.cfg.ServerHTTP+"/api/device/register", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("register status %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Status == "approved", nil
}

func (c *Client) runWS() error {
	u, err := url.Parse(c.cfg.ServerWS)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("device_id", c.cfg.DeviceID)
	u.RawQuery = q.Encode()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.cfg.DeviceSecret)
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		return err
	}
	defer conn.Close()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"status"}`))
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var cmd map[string]any
		if err := json.Unmarshal(data, &cmd); err != nil {
			continue
		}
		c.handleCommand(cmd)
	}
}

func (c *Client) handleCommand(cmd map[string]any) {
	cmdType, _ := cmd["type"].(string)
	switch cmdType {
	case "capture_photo":
		if path, err := c.capturePhoto(); err == nil {
			c.enqueue(path, "photo")
		}
	case "start_record":
		_ = c.startRecord()
	case "stop_record":
		_ = c.stopRecord()
	case "sync_schedules":
		raw, _ := json.Marshal(cmd["schedules"])
		var schedules []Schedule
		_ = json.Unmarshal(raw, &schedules)
		c.applySchedules(schedules)
	}
}

func (c *Client) applySchedules(list []Schedule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.schedulers {
		cancel()
	}
	c.schedulers = map[string]context.CancelFunc{}
	for _, s := range list {
		if !s.Enabled || s.IntervalSeconds <= 0 {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		c.schedulers[s.ID] = cancel
		go c.runSchedule(ctx, s)
	}
}

func (c *Client) runSchedule(ctx context.Context, s Schedule) {
	t := time.NewTicker(time.Duration(s.IntervalSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			switch s.Type {
			case "capture_photo":
				if path, err := c.capturePhoto(); err == nil {
					c.enqueue(path, "photo")
				}
			case "record_cycle":
				if err := c.startRecord(); err == nil {
					time.Sleep(time.Duration(max(5, s.DurationSeconds)) * time.Second)
					_ = c.stopRecord()
				}
			}
		}
	}
}

func (c *Client) capturePhoto() (string, error) {
	name := fmt.Sprintf("%s_photo_%s.jpg", c.cfg.DeviceID, time.Now().UTC().Format("20060102T150405"))
	path := filepath.Join(c.cfg.OutboxDir, name)
	inputArgs, inputErr := cameraInputArgs(c.cfg.CameraDevice)
	if inputErr != nil {
		return "", inputErr
	}
	args := append([]string{"-y"}, inputArgs...)
	args = append(args, "-frames:v", "1", path)
	cmd := exec.Command("ffmpeg", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ffmpeg photo: %w (%s)", err, string(out))
	}
	_ = c.ensureQuota()
	return path, nil
}

func (c *Client) startRecord() error {
	c.mu.Lock()
	if c.recordSession != nil {
		c.mu.Unlock()
		return errors.New("already recording")
	}
	inputArgs, inputErr := cameraInputArgs(c.cfg.CameraDevice)
	if inputErr != nil {
		c.mu.Unlock()
		return inputErr
	}
	name := fmt.Sprintf("%s_video_%s.mp4", c.cfg.DeviceID, time.Now().UTC().Format("20060102T150405"))
	path := filepath.Join(c.cfg.OutboxDir, name)
	args := append([]string{"-y"}, inputArgs...)
	args = append(args,
		"-an",
		"-c:v", "libx264",
		"-profile:v", "main",
		"-level", "4.0",
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		"-f", "mp4",
		path,
	)
	cmd := exec.Command("ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		c.mu.Unlock()
		return err
	}
	sess := &recordSession{cmd: cmd, stdin: stdin, path: path}
	c.recordSession = sess
	c.mu.Unlock()

	go func(s *recordSession) {
		_ = s.cmd.Wait()
		c.mu.Lock()
		if c.recordSession == s {
			c.recordSession = nil
		}
		c.mu.Unlock()
		if _, statErr := os.Stat(s.path); statErr == nil {
			c.enqueue(s.path, "video")
		}
	}(sess)
	return nil
}

func (c *Client) stopRecord() error {
	c.mu.Lock()
	sess := c.recordSession
	c.mu.Unlock()
	if sess == nil {
		return errors.New("not recording")
	}
	if _, werr := io.WriteString(sess.stdin, "q\n"); werr != nil {
		_ = sess.cmd.Process.Kill()
	}
	_ = sess.stdin.Close()
	return nil
}

func (c *Client) uploadLoop() {
	for {
		items, _ := os.ReadDir(c.cfg.OutboxDir)
		sort.Slice(items, func(i, j int) bool {
			ii, _ := items[i].Info()
			jj, _ := items[j].Info()
			return ii.ModTime().Before(jj.ModTime())
		})
		for _, e := range items {
			if e.IsDir() {
				continue
			}
			path := filepath.Join(c.cfg.OutboxDir, e.Name())
			kind := "photo"
			if strings.Contains(e.Name(), "_video_") || strings.HasSuffix(strings.ToLower(e.Name()), ".mp4") {
				kind = "video"
			}
			if err := c.uploadFile(path, kind); err == nil {
				_ = os.Remove(path)
			} else {
				// Unauthorized often means approval revoked; force re-register cycle.
				if strings.Contains(err.Error(), "401") {
					return
				}
				break
			}
		}
		_ = c.ensureQuota()
		time.Sleep(5 * time.Second)
	}
}

func (c *Client) uploadFile(path, kind string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		return err
	}
	_ = w.Close()

	url := c.cfg.ServerHTTP + "/api/device/upload/" + kind
	req, _ := http.NewRequest(http.MethodPost, url, &body)
	req.Header.Set("Authorization", "Bearer "+c.cfg.DeviceSecret)
	req.Header.Set("X-Device-ID", c.cfg.DeviceID)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) enqueue(path, _ string) {
	_ = c.ensureQuota()
	log.Printf("queued file %s", path)
}

func (c *Client) ensureQuota() error {
	entries, err := os.ReadDir(c.cfg.OutboxDir)
	if err != nil {
		return err
	}
	type item struct {
		path string
		size int64
		t    time.Time
	}
	files := []item{}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, item{
			path: filepath.Join(c.cfg.OutboxDir, e.Name()),
			size: info.Size(),
			t:    info.ModTime(),
		})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].t.Before(files[j].t) })
	for total > c.cfg.MaxStorageBytes && len(files) > 0 {
		f := files[0]
		files = files[1:]
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
	}
	return nil
}

func parseInt64(v string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(v, "%d", &n)
	return n, err
}

func env(k, d string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return d
	}
	return v
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func shortRandom() string {
	return randomHex(4)
}

func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "homeguard"
	}
	replacer := strings.NewReplacer(" ", "-", "_", "-", ".", "-", "/", "-", "\\", "-")
	return replacer.Replace(s)
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func cameraInputArgs(cameraDevice string) ([]string, error) {
	cameraDevice = strings.TrimSpace(cameraDevice)
	if cameraDevice == "" {
		return nil, errors.New("camera_device is empty")
	}
	switch runtime.GOOS {
	case "linux":
		return []string{"-f", "v4l2", "-i", cameraDevice}, nil
	case "windows":
		if strings.HasPrefix(cameraDevice, "/dev/video") {
			return nil, errors.New("windows does not support /dev/video*; set camera_device to dshow name")
		}
		return []string{"-f", "dshow", "-i", "video=" + cameraDevice}, nil
	default:
		return nil, fmt.Errorf("unsupported OS for camera capture: %s", runtime.GOOS)
	}
}
