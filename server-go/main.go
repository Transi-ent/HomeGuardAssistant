package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/crypto/bcrypt"
)

const (
	bucketUsers     = "users"
	bucketDevices   = "devices"
	bucketSchedules = "schedules"
	bucketAudit     = "audit"
	sessionTTL      = 7 * 24 * time.Hour

	devicePending  = "pending"
	deviceApproved = "approved"
	deviceRejected = "rejected"
)

type Config struct {
	Addr          string
	DataDir       string
	MediaDir      string
	DefaultUser   string
	DefaultPass   string
	SessionSecret string
}

type DeviceConn struct {
	ID   string
	Conn *websocket.Conn
	Mu   sync.Mutex
}

type App struct {
	cfg      Config
	db       *bolt.DB
	tmpl     *template.Template
	upgrader websocket.Upgrader
	devMu    sync.RWMutex
	devConns map[string]*DeviceConn
}

type DeviceRecord struct {
	ID         string `json:"id"`
	Secret     string `json:"secret,omitempty"`
	SecretHash string `json:"secret_hash,omitempty"`
	Name       string `json:"name"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Status     string `json:"status"`
	LastSeen   int64  `json:"last_seen"`
	CreatedAt  int64  `json:"created_at"`
}

type AuditEvent struct {
	ID        string `json:"id"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
	CreatedAt int64  `json:"created_at"`
}

type DeviceView struct {
	ID        string
	Name      string
	OS        string
	Arch      string
	Status    string
	Online    bool
	LastSeenS string
}

type Schedule struct {
	ID              string `json:"id"`
	DeviceID        string `json:"device_id"`
	Type            string `json:"type"`
	IntervalSeconds int64  `json:"interval_seconds"`
	DurationSeconds int64  `json:"duration_seconds"`
	Enabled         bool   `json:"enabled"`
}

type MediaItem struct {
	Name    string
	URL     string
	Type    string
	SizeMB  string
	Created string
}

func main() {
	cfg := Config{
		Addr:          env("HG_ADDR", ":38080"),
		DataDir:       env("HG_DATA_DIR", "./data"),
		MediaDir:      env("HG_MEDIA_DIR", "./data/media"),
		DefaultUser:   env("HG_DEFAULT_USER", "admin"),
		DefaultPass:   env("HG_DEFAULT_PASS", "12345678"),
		SessionSecret: env("HG_SESSION_SECRET", "change-me-in-production"),
	}
	if err := os.MkdirAll(cfg.MediaDir, 0o755); err != nil {
		log.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(cfg.DataDir, "homeguard.db"), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	tmpl, err := template.ParseFiles("templates/index.html")
	if err != nil {
		log.Fatal(err)
	}

	app := &App{
		cfg:  cfg,
		db:   db,
		tmpl: tmpl,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		devConns: map[string]*DeviceConn{},
	}
	if err := app.initDB(); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.Handle("/media/", http.StripPrefix("/media/", http.FileServer(http.Dir(cfg.MediaDir))))
	mux.HandleFunc("/login", app.loginHandler)
	mux.HandleFunc("/logout", app.logoutHandler)

	mux.HandleFunc("/api/device/register", app.deviceRegisterHandler)
	mux.HandleFunc("/ws/device", app.deviceWSHandler)
	mux.HandleFunc("/api/device/upload/photo", app.deviceUploadHandler("photo"))
	mux.HandleFunc("/api/device/upload/video", app.deviceUploadHandler("video"))

	mux.HandleFunc("/api/console/password", app.auth(app.consolePasswordHandler))
	mux.HandleFunc("/api/console/command", app.auth(app.consoleCommandHandler))
	mux.HandleFunc("/api/console/schedules", app.auth(app.consoleSchedulesHandler))
	mux.HandleFunc("/api/console/media", app.auth(app.consoleMediaHandler))
	mux.HandleFunc("/api/console/device-requests", app.auth(app.consoleDeviceRequestsHandler))
	mux.HandleFunc("/api/console/device-requests/approve", app.auth(app.consoleApproveDeviceHandler))
	mux.HandleFunc("/api/console/device-requests/reject", app.auth(app.consoleRejectDeviceHandler))
	mux.HandleFunc("/api/console/audit", app.auth(app.consoleAuditHandler))

	mux.HandleFunc("/", app.auth(app.indexHandler))

	log.Printf("server listening on %s", cfg.Addr)
	log.Fatal(http.ListenAndServe(cfg.Addr, mux))
}

func (a *App) initDB() error {
	return a.db.Update(func(tx *bolt.Tx) error {
		for _, b := range []string{bucketUsers, bucketDevices, bucketSchedules, bucketAudit} {
			if _, err := tx.CreateBucketIfNotExists([]byte(b)); err != nil {
				return err
			}
		}
		users := tx.Bucket([]byte(bucketUsers))
		if users.Get([]byte(a.cfg.DefaultUser)) == nil {
			hash, err := bcrypt.GenerateFromPassword([]byte(a.cfg.DefaultPass), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			return users.Put([]byte(a.cfg.DefaultUser), hash)
		}
		return nil
	})
}

func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("hg_session")
		if err != nil || !a.verifySession(c.Value) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func (a *App) loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		io.WriteString(w, `<html><body><h2>HomeGuard Login</h2><form method="post"><input name="username" placeholder="username"/><br/><input name="password" type="password" placeholder="password"/><br/><button>Login</button></form></body></html>`)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	ok := false
	_ = a.db.View(func(tx *bolt.Tx) error {
		hash := tx.Bucket([]byte(bucketUsers)).Get([]byte(username))
		if hash == nil {
			return nil
		}
		ok = bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
		return nil
	})
	if !ok {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "hg_session",
		Value:    a.signSession(username),
		HttpOnly: true,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *App) logoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "hg_session", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (a *App) indexHandler(w http.ResponseWriter, r *http.Request) {
	media, _ := a.listMedia()
	devices, _ := a.listDevices()
	schedules, _ := a.listSchedules("")
	pending := make([]DeviceRecord, 0)
	for _, d := range devices {
		if d.Status == devicePending {
			pending = append(pending, d)
		}
	}
	view := map[string]any{
		"Media":        media,
		"Devices":      a.buildDeviceViews(devices),
		"Schedules":    schedules,
		"Pending":      pending,
		"PendingCount": len(pending),
		"MediaCount":   len(media),
		"AuditEvents":  mustAudit(a.listAudit(50)),
	}
	if err := a.tmpl.Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) deviceRegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		DeviceID     string `json:"device_id"`
		DeviceSecret string `json:"device_secret"`
		DeviceName   string `json:"device_name"`
		OS           string `json:"os"`
		Arch         string `json:"arch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	in.DeviceID = strings.TrimSpace(in.DeviceID)
	in.DeviceSecret = strings.TrimSpace(in.DeviceSecret)
	if in.DeviceID == "" || in.DeviceSecret == "" {
		http.Error(w, "device_id and device_secret are required", http.StatusBadRequest)
		return
	}
	now := time.Now().Unix()
	out, err := a.upsertDeviceRegistration(DeviceRecord{
		ID:        in.DeviceID,
		Secret:    in.DeviceSecret,
		Name:      strings.TrimSpace(in.DeviceName),
		OS:        strings.TrimSpace(in.OS),
		Arch:      strings.TrimSpace(in.Arch),
		LastSeen:  now,
		CreatedAt: now,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	_ = a.appendAudit("device:"+out.ID, "register", out.ID, "registration request processed with status "+out.Status)
	msg := "pending approval"
	if out.Status == deviceApproved {
		msg = "approved"
	}
	if out.Status == deviceRejected {
		msg = "registration rejected"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     out.Status,
		"message":    msg,
		"device_id":  out.ID,
		"deviceName": out.Name,
	})
}

func (a *App) consoleDeviceRequestsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	devs, _ := a.listDevices()
	out := make([]DeviceRecord, 0)
	for _, d := range devs {
		if d.Status == devicePending {
			out = append(out, d)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) consoleApproveDeviceHandler(w http.ResponseWriter, r *http.Request) {
	a.updateDeviceStatusHandler(w, r, deviceApproved)
}

func (a *App) consoleRejectDeviceHandler(w http.ResponseWriter, r *http.Request) {
	a.updateDeviceStatusHandler(w, r, deviceRejected)
}

func (a *App) updateDeviceStatusHandler(w http.ResponseWriter, r *http.Request, status string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	in.DeviceID = strings.TrimSpace(in.DeviceID)
	if in.DeviceID == "" {
		http.Error(w, "device_id required", http.StatusBadRequest)
		return
	}
	d, err := a.getDevice(in.DeviceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	d.Status = status
	if err := a.saveDevice(d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = a.appendAudit("console", "device_status_update", d.ID, status)
	writeJSON(w, http.StatusOK, d)
}

func (a *App) consoleAuditHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 100
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	events, err := a.listAudit(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (a *App) consolePasswordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Username    string `json:"username"`
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || in.NewPassword == "" || in.OldPassword == "" {
		http.Error(w, "username, old_password and new_password are required", http.StatusBadRequest)
		return
	}
	ok := false
	err := a.db.Update(func(tx *bolt.Tx) error {
		users := tx.Bucket([]byte(bucketUsers))
		hash := users.Get([]byte(in.Username))
		if hash == nil {
			return nil
		}
		if bcrypt.CompareHashAndPassword(hash, []byte(in.OldPassword)) != nil {
			return nil
		}
		newHash, err := bcrypt.GenerateFromPassword([]byte(in.NewPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		ok = true
		return users.Put([]byte(in.Username), newHash)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "invalid username or old_password", http.StatusUnauthorized)
		return
	}
	_ = a.appendAudit("console", "password_change", in.Username, "changed console password")
	io.WriteString(w, "ok")
}

func (a *App) consoleCommandHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in map[string]any
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	deviceID, _ := in["device_id"].(string)
	cmdType, _ := in["type"].(string)
	if deviceID == "" || cmdType == "" {
		http.Error(w, "device_id and type are required", http.StatusBadRequest)
		return
	}
	if err := a.sendCommand(deviceID, in); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	io.WriteString(w, "ok")
}

func (a *App) consoleSchedulesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		deviceID := r.URL.Query().Get("device_id")
		s, _ := a.listSchedules(deviceID)
		writeJSON(w, http.StatusOK, s)
	case http.MethodPost:
		var s Schedule
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if s.ID == "" {
			s.ID = fmt.Sprintf("sch_%d", time.Now().UnixNano())
		}
		if s.IntervalSeconds <= 0 {
			http.Error(w, "interval_seconds must be > 0", http.StatusBadRequest)
			return
		}
		s.Enabled = true
		if err := a.saveSchedule(s); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = a.sendCommand(s.DeviceID, map[string]any{"type": "sync_schedules"})
		writeJSON(w, http.StatusCreated, s)
	case http.MethodPatch:
		var in struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s, err := a.getSchedule(in.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		s.Enabled = in.Enabled
		if err := a.saveSchedule(s); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = a.sendCommand(s.DeviceID, map[string]any{"type": "sync_schedules"})
		writeJSON(w, http.StatusOK, s)
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}
		s, err := a.getSchedule(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err := a.deleteSchedule(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = a.sendCommand(s.DeviceID, map[string]any{"type": "sync_schedules"})
		io.WriteString(w, "ok")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) consoleMediaHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := a.listMedia()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		if filepath.Base(name) != name || strings.Contains(name, "..") {
			http.Error(w, "invalid file name", http.StatusBadRequest)
			return
		}
		target := filepath.Join(a.cfg.MediaDir, name)
		if err := os.Remove(target); err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = a.appendAudit("console", "media_delete", name, "deleted media file")
		io.WriteString(w, "ok")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) deviceWSHandler(w http.ResponseWriter, r *http.Request) {
	deviceID := r.URL.Query().Get("device_id")
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !a.verifyApprovedDevice(deviceID, secret) {
		http.Error(w, "device not approved or auth failed", http.StatusUnauthorized)
		return
	}
	conn, err := a.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	dc := &DeviceConn{ID: deviceID, Conn: conn}
	a.devMu.Lock()
	a.devConns[deviceID] = dc
	a.devMu.Unlock()
	defer func() {
		a.devMu.Lock()
		delete(a.devConns, deviceID)
		a.devMu.Unlock()
		_ = conn.Close()
	}()
	_ = a.touchDevice(deviceID)
	_ = a.sendCommand(deviceID, map[string]any{"type": "sync_schedules"})
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = a.touchDevice(deviceID)
		log.Printf("device %s -> %s", deviceID, string(data))
	}
}

func (a *App) deviceUploadHandler(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.Header.Get("X-Device-ID")
		secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !a.verifyApprovedDevice(deviceID, secret) {
			http.Error(w, "device not approved or auth failed", http.StatusUnauthorized)
			return
		}
		if err := r.ParseMultipartForm(1 << 30); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		ext := filepath.Ext(header.Filename)
		if ext == "" {
			if kind == "photo" {
				ext = ".jpg"
			} else {
				ext = ".mp4"
			}
		}
		ts := time.Now().UTC().Format("20060102T150405")
		name := fmt.Sprintf("%s_%s_%s%s", deviceID, kind, ts, ext)
		dst := filepath.Join(a.cfg.MediaDir, name)
		out, err := os.Create(dst)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer out.Close()
		if _, err := io.Copy(out, file); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = a.touchDevice(deviceID)
		io.WriteString(w, "ok")
	}
}

func (a *App) sendCommand(deviceID string, msg map[string]any) error {
	d, err := a.getDevice(deviceID)
	if err != nil {
		return err
	}
	if d.Status != deviceApproved {
		return errors.New("device is not approved")
	}
	a.devMu.RLock()
	dc := a.devConns[deviceID]
	a.devMu.RUnlock()
	if dc == nil {
		return errors.New("device is offline")
	}
	if msg["type"] == "sync_schedules" {
		s, _ := a.listSchedules(deviceID)
		msg["schedules"] = s
	}
	data, _ := json.Marshal(msg)
	dc.Mu.Lock()
	defer dc.Mu.Unlock()
	return dc.Conn.WriteMessage(websocket.TextMessage, data)
}

func (a *App) verifyApprovedDevice(deviceID, secret string) bool {
	d, err := a.getDevice(deviceID)
	if err != nil {
		return false
	}
	return deviceSecretMatches(d, secret) && d.Status == deviceApproved
}

func (a *App) getDevice(deviceID string) (DeviceRecord, error) {
	var out DeviceRecord
	err := a.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(bucketDevices)).Get([]byte(deviceID))
		if raw == nil {
			return errors.New("device not found")
		}
		return json.Unmarshal(raw, &out)
	})
	return out, err
}

func (a *App) upsertDeviceRegistration(in DeviceRecord) (DeviceRecord, error) {
	var out DeviceRecord
	err := a.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketDevices))
		raw := b.Get([]byte(in.ID))
		if raw == nil {
			in.Status = devicePending
			in.SecretHash = hashDeviceSecret(in.Secret)
			in.Secret = ""
			out = in
			data, _ := json.Marshal(out)
			return b.Put([]byte(out.ID), data)
		}
		var ex DeviceRecord
		if err := json.Unmarshal(raw, &ex); err != nil {
			return err
		}
		if !deviceSecretMatches(ex, in.Secret) {
			return errors.New("device secret mismatch")
		}
		if ex.SecretHash == "" {
			ex.SecretHash = hashDeviceSecret(in.Secret)
			ex.Secret = ""
		}
		ex.Name = in.Name
		ex.OS = in.OS
		ex.Arch = in.Arch
		ex.LastSeen = in.LastSeen
		out = ex
		data, _ := json.Marshal(out)
		return b.Put([]byte(out.ID), data)
	})
	return out, err
}

func (a *App) saveDevice(d DeviceRecord) error {
	return a.db.Update(func(tx *bolt.Tx) error {
		data, _ := json.Marshal(d)
		return tx.Bucket([]byte(bucketDevices)).Put([]byte(d.ID), data)
	})
}

func (a *App) touchDevice(deviceID string) error {
	return a.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketDevices))
		raw := b.Get([]byte(deviceID))
		if raw == nil {
			return nil
		}
		var rec DeviceRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return err
		}
		rec.LastSeen = time.Now().Unix()
		data, _ := json.Marshal(rec)
		return b.Put([]byte(deviceID), data)
	})
}

func (a *App) listDevices() ([]DeviceRecord, error) {
	out := []DeviceRecord{}
	err := a.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketDevices)).ForEach(func(_, v []byte) error {
			var rec DeviceRecord
			if err := json.Unmarshal(v, &rec); err == nil {
				out = append(out, rec)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, err
}

func (a *App) saveSchedule(s Schedule) error {
	return a.db.Update(func(tx *bolt.Tx) error {
		data, _ := json.Marshal(s)
		return tx.Bucket([]byte(bucketSchedules)).Put([]byte(s.ID), data)
	})
}

func (a *App) getSchedule(id string) (Schedule, error) {
	var out Schedule
	err := a.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketSchedules)).Get([]byte(id))
		if v == nil {
			return errors.New("schedule not found")
		}
		return json.Unmarshal(v, &out)
	})
	return out, err
}

func (a *App) deleteSchedule(id string) error {
	return a.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketSchedules)).Delete([]byte(id))
	})
}

func (a *App) listSchedules(deviceID string) ([]Schedule, error) {
	out := []Schedule{}
	err := a.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketSchedules)).ForEach(func(_, v []byte) error {
			var s Schedule
			if json.Unmarshal(v, &s) == nil {
				if deviceID == "" || s.DeviceID == deviceID {
					out = append(out, s)
				}
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

func (a *App) listMedia() ([]MediaItem, error) {
	entries, err := os.ReadDir(a.cfg.MediaDir)
	if err != nil {
		return nil, err
	}
	items := make([]MediaItem, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := e.Name()
		typ := "photo"
		if strings.Contains(name, "_video_") || strings.HasSuffix(strings.ToLower(name), ".mp4") {
			typ = "video"
		}
		items = append(items, MediaItem{
			Name:    name,
			URL:     "/media/" + name,
			Type:    typ,
			SizeMB:  fmt.Sprintf("%.2f", float64(info.Size())/1024/1024),
			Created: info.ModTime().Format(time.RFC3339),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Created > items[j].Created })
	return items, nil
}

func (a *App) appendAudit(actor, action, target, detail string) error {
	ev := AuditEvent{
		ID:        fmt.Sprintf("ae_%d", time.Now().UnixNano()),
		Actor:     actor,
		Action:    action,
		Target:    target,
		Detail:    detail,
		CreatedAt: time.Now().Unix(),
	}
	return a.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketAudit))
		raw, _ := json.Marshal(ev)
		return b.Put([]byte(ev.ID), raw)
	})
}

func (a *App) listAudit(limit int) ([]AuditEvent, error) {
	out := []AuditEvent{}
	err := a.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketAudit)).ForEach(func(_, v []byte) error {
			var ev AuditEvent
			if err := json.Unmarshal(v, &ev); err == nil {
				out = append(out, ev)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

func hashDeviceSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func deviceSecretMatches(d DeviceRecord, plain string) bool {
	if plain == "" {
		return false
	}
	if d.SecretHash != "" {
		return d.SecretHash == hashDeviceSecret(plain)
	}
	return d.Secret == plain
}

func (a *App) buildDeviceViews(devices []DeviceRecord) []DeviceView {
	out := make([]DeviceView, 0, len(devices))
	now := time.Now().Unix()
	a.devMu.RLock()
	defer a.devMu.RUnlock()
	for _, d := range devices {
		_, connected := a.devConns[d.ID]
		online := d.Status == deviceApproved && (connected || (now-d.LastSeen <= 45 && d.LastSeen > 0))
		lastSeenS := "-"
		if d.LastSeen > 0 {
			lastSeenS = time.Unix(d.LastSeen, 0).Format(time.RFC3339)
		}
		out = append(out, DeviceView{
			ID:        d.ID,
			Name:      d.Name,
			OS:        d.OS,
			Arch:      d.Arch,
			Status:    d.Status,
			Online:    online,
			LastSeenS: lastSeenS,
		})
	}
	return out
}

func env(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func mustAudit(events []AuditEvent, err error) []AuditEvent {
	if err != nil {
		return []AuditEvent{}
	}
	return events
}

func (a *App) signSession(username string) string {
	payload := username + "|" + strconv.FormatInt(time.Now().Unix(), 10)
	sig := hmacSHA256(payload, a.cfg.SessionSecret)
	return payload + "|" + sig
}

func (a *App) verifySession(token string) bool {
	parts := strings.Split(token, "|")
	if len(parts) != 3 {
		return false
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	if time.Since(time.Unix(ts, 0)) > sessionTTL {
		return false
	}
	want := hmacSHA256(parts[0]+"|"+parts[1], a.cfg.SessionSecret)
	return hmac.Equal([]byte(want), []byte(parts[2]))
}

func hmacSHA256(payload, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}
