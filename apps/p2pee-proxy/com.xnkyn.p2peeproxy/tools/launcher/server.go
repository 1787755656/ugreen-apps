package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Broadcaster fans out captured log lines to SSE subscribers.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan string]bool
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: map[chan string]bool{}}
}

func (b *Broadcaster) Subscribe() chan string {
	ch := make(chan string, 64)
	b.mu.Lock()
	b.subs[ch] = true
	b.mu.Unlock()
	return ch
}

func (b *Broadcaster) Unsubscribe(ch chan string) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *Broadcaster) Publish(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- line:
		default: // drop slow subscriber
		}
	}
}

// ---- local-request gate (UGOS login two-gate, first fence) ----

var localIPs = computeLocalIPs()

func computeLocalIPs() map[string]bool {
	set := map[string]bool{}
	ifs, err := net.Interfaces()
	if err != nil {
		return set
	}
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			set[ipnet.IP.String()] = true
			if v4 := ipnet.IP.To4(); v4 != nil {
				set[v4.String()] = true
			}
		}
	}
	return set
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLocalRequest(r *http.Request) bool {
	// Do NOT trust X-Forwarded-For: the gateway connects from a NAS-local
	// address, and a LAN client's browser IP must never be accepted.
	ip := net.ParseIP(clientIP(r))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	if localIPs[ip.String()] {
		return true
	}
	if v4 := ip.To4(); v4 != nil && localIPs[v4.String()] {
		return true
	}
	return false
}

func gate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocalRequest(r) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("此页面不对外提供访问，请从 NAS 桌面的应用图标打开。"))
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Server wires the HTTP API.
type Server struct {
	mgr     *Manager
	www     string
	session *SessionStore
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/healthz", s.handleHealthz)
	// auth-state probe + login/setup/logout are reachable by any local user.
	mux.HandleFunc("/api/auth", gate(s.handleAuth))
	mux.HandleFunc("/api/setup", gate(s.handleSetup))
	mux.HandleFunc("/api/login", gate(s.handleLogin))
	mux.HandleFunc("/api/logout", gate(s.handleLogout))
	// everything else requires local access AND a valid session (if a
	// password is configured).
	mux.HandleFunc("/api/diag", s.authGate(s.handleDiag))
	mux.HandleFunc("/api/config", s.authGate(s.handleConfig))
	mux.HandleFunc("/api/start", s.authGate(s.handleStart))
	mux.HandleFunc("/api/stop", s.authGate(s.handleStop))
	mux.HandleFunc("/api/status", s.authGate(s.handleStatus))
	mux.HandleFunc("/api/logs", s.authGate(s.handleLogs))
	if s.www != "" {
		fs := http.FileServer(http.Dir(s.www))
		// the page itself is served to any local user; the embedded JS shows
		// the login/setup form when the API returns 401.
		mux.Handle("/", gate(fs.ServeHTTP))
	}
	return mux
}

// authGate enforces the two gates: must be a NAS-local request, and — once a
// password is set — must carry a valid session cookie.
func (s *Server) authGate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocalRequest(r) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("此页面不对外提供访问，请从 NAS 桌面的应用图标打开。"))
			return
		}
		if s.mgr.HasPassword() && !s.session.Valid(sessionToken(r)) {
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "unauthorized", "setup": false})
			return
		}
		h(w, r)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// handleAuth reports whether the app still needs a first-run password setup
// and whether the current request is already authenticated.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"setup":         !s.mgr.HasPassword(),
		"authenticated": s.session.Valid(sessionToken(r)),
	})
}

// handleSetup sets the management password on first run.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.mgr.HasPassword() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "密码已设置，请直接登录。"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Password) < 4 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "密码至少 4 位。"})
		return
	}
	if err := s.mgr.SetPassword(body.Password); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.session.setCookie(w, s.session.New())
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleLogin verifies the management password and issues a session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if !s.mgr.HasPassword() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "尚未设置密码，请先设置。"})
		return
	}
	if !s.mgr.CheckPassword(body.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "密码错误"})
		return
	}
	s.session.setCookie(w, s.session.New())
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleLogout revokes the current session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.session.Revoke(sessionToken(r))
	s.session.clearCookie(w)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request) {
	hdrs := map[string]string{}
	for k := range r.Header {
		if k == "Ugreen-Ttk" {
			if v := r.Header.Get(k); v != "" {
				hdrs[k] = fmt.Sprintf("<len=%d>", len(v))
			}
			continue
		}
		hdrs[k] = r.Header.Get(k)
	}
	ips := make([]string, 0, len(localIPs))
	for ip := range localIPs {
		ips = append(ips, ip)
	}
	writeJSON(w, 200, map[string]interface{}{
		"remoteAddr": r.RemoteAddr,
		"localIPs":   ips,
		"headers":    hdrs,
		"running":    s.mgr.IsRunning(),
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.mgr.mu.Lock()
		cfg := s.mgr.cfg
		s.mgr.mu.Unlock()
		// Never return the password hash to the client.
		writeJSON(w, 200, map[string]interface{}{
			"key":         cfg.Key,
			"targets":     cfg.Targets,
			"tlsInsecure": cfg.TLSInsecure,
		})
		return
	}
	if r.Method == http.MethodPost {
		var c Config
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		s.mgr.mu.Lock()
		s.mgr.cfg.Key = strings.TrimSpace(c.Key)
		s.mgr.cfg.Targets = c.Targets
		s.mgr.cfg.TLSInsecure = c.TLSInsecure
		s.mgr.mu.Unlock()
		s.mgr.SaveConfig()
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := s.mgr.Start(); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := s.mgr.Stop(); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mgr.mu.Lock()
	cfg := s.mgr.cfg
	s.mgr.mu.Unlock()
	writeJSON(w, 200, map[string]interface{}{
		"running":     s.mgr.IsRunning(),
		"pid":         s.mgr.PID(),
		"keyMasked":   maskKey(cfg.Key),
		"targets":     cfg.Targets,
		"tlsInsecure": cfg.TLSInsecure,
		"accessURLs":  s.mgr.AccessURLs(),
		"log":         s.mgr.RecentLog(),
	})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.mgr.broadcaster.Subscribe()
	defer s.mgr.broadcaster.Unsubscribe(ch)

	// Backfill current snapshot first.
	for _, l := range s.mgr.RecentLog() {
		fmt.Fprintf(w, "data: %d\t%s\n\n", l.TS, l.Text)
	}
	for _, u := range s.mgr.AccessURLs() {
		fmt.Fprintf(w, "event: url\ndata: %s\n\n", u)
	}
	flusher.Flush()

	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return
			}
			if strings.HasPrefix(line, "URL\t") {
				fmt.Fprintf(w, "event: url\ndata: %s\n\n", strings.TrimPrefix(line, "URL\t"))
			} else {
				fmt.Fprintf(w, "data: %s\n\n", line)
			}
			flusher.Flush()
		case <-keep.C:
			fmt.Fprintf(w, ":\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
