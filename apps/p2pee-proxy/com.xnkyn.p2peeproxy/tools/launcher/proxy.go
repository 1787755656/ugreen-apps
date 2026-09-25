package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Target is a single forwarding rule. Name is optional (used as the
// "label=url" form accepted by p2pee-proxy).
type Target struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Config is the persisted manager configuration.
type Config struct {
	Key         string   `json:"key"`
	Targets     []Target `json:"targets"`
	TLSInsecure bool     `json:"tlsInsecure"`
	Running     bool     `json:"running"`
	// PasswordHash holds a PBKDF2 hash. Empty means the app is in first-run
	// setup mode and no login is required yet. Never serialized to clients.
	PasswordHash string `json:"passwordHash"`
}

// LogLine is one captured line from the proxy's stdout/stderr.
type LogLine struct {
	TS   int64  `json:"ts"`
	Text string `json:"text"`
}

var urlRe = regexp.MustCompile(`https?://[A-Za-z0-9._~:/?=&#%+\-@]+`)

func extractURLs(s string) []string {
	raw := urlRe.FindAllString(s, -1)
	out := make([]string, 0, len(raw))
	for _, u := range raw {
		u = strings.TrimRight(u, `.,;:'"`)
		if len(u) > 12 { // ignore trivially short false positives
			out = append(out, u)
		}
	}
	return out
}

// Manager owns the lifecycle of the bundled p2pee-proxy process, captures its
// output, and exposes a status snapshot.
type Manager struct {
	mu          sync.Mutex
	proxyPath   string
	dataDir     string
	tmpDir      string
	cfg         Config
	cmd         *exec.Cmd
	ring        []LogLine
	ringMax     int
	accessURLs  []string
	urlSeen     map[string]bool
	broadcaster *Broadcaster
}

func NewManager(proxyPath, dataDir string) *Manager {
	return &Manager{
		proxyPath:   proxyPath,
		dataDir:     dataDir,
		tmpDir:      filepath.Join(dataDir, "tmp"),
		ringMax:     500,
		accessURLs:  []string{},
		urlSeen:     map[string]bool{},
		broadcaster: NewBroadcaster(),
	}
}

func (m *Manager) LoadConfig() {
	p := filepath.Join(m.dataDir, "config.json")
	b, err := os.ReadFile(p)
	if err == nil {
		_ = json.Unmarshal(b, &m.cfg)
	}
}

func (m *Manager) SaveConfig() {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	_ = os.MkdirAll(m.dataDir, 0o755)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(m.dataDir, "config.json"), b, 0o644)
}

// lineWriter buffers partial lines and forwards complete lines to fn.
type lineWriter struct {
	fn  func(string)
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]
		w.fn(line)
	}
	return len(p), nil
}

func (m *Manager) processLine(text string) {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return
	}
	line := LogLine{TS: time.Now().Unix(), Text: text}

	m.mu.Lock()
	m.ring = append(m.ring, line)
	if len(m.ring) > m.ringMax {
		m.ring = m.ring[len(m.ring)-m.ringMax:]
	}
	for _, u := range extractURLs(text) {
		if !m.urlSeen[u] {
			m.urlSeen[u] = true
			m.accessURLs = append(m.accessURLs, u)
			if len(m.accessURLs) > 30 {
				m.accessURLs = m.accessURLs[1:]
			}
			m.broadcaster.Publish("URL\t" + u)
		}
	}
	m.mu.Unlock()

	m.broadcaster.Publish(fmt.Sprintf("%d\t%s", line.TS, line.Text))
}

// Start launches the proxy using the current configuration.
func (m *Manager) Start() error {
	m.mu.Lock()
	if m.cmd != nil && m.cmd.Process != nil {
		m.mu.Unlock()
		return fmt.Errorf("已在运行")
	}
	if strings.TrimSpace(m.cfg.Key) == "" {
		m.mu.Unlock()
		return fmt.Errorf("未配置 Key")
	}

	args := []string{"-key", strings.TrimSpace(m.cfg.Key)}
	for _, t := range m.cfg.Targets {
		u := strings.TrimSpace(t.URL)
		if u == "" {
			continue
		}
		name := strings.TrimSpace(t.Name)
		if name != "" {
			args = append(args, "-target", name+"="+u)
		} else {
			args = append(args, "-target", u)
		}
	}
	if m.cfg.TLSInsecure {
		args = append(args, "-tls-insecure")
	}

	if err := os.MkdirAll(m.tmpDir, 0o755); err != nil {
		m.mu.Unlock()
		return err
	}

	cmd := exec.Command(m.proxyPath, args...)
	cmd.Env = append(os.Environ(), "TMPDIR="+m.tmpDir, "HOME="+m.dataDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	lw := &lineWriter{fn: m.processLine}
	cmd.Stdout = lw
	cmd.Stderr = lw

	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.cmd = cmd
	m.cfg.Running = true
	m.mu.Unlock()

	m.SaveConfig()
	m.processLine("[manager] started proxy, pid=" + fmt.Sprint(cmd.Process.Pid))

	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		m.cfg.Running = false
		m.cmd = nil
		m.mu.Unlock()
		m.processLine("[manager] proxy process exited")
		m.SaveConfig()
	}()

	return nil
}

// Stop terminates the proxy process group gracefully.
func (m *Manager) Stop() error {
	m.mu.Lock()
	cmd := m.cmd
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("未在运行")
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	m.mu.Lock()
	m.cfg.Running = false
	m.cmd = nil
	m.mu.Unlock()
	m.SaveConfig()
	m.processLine("[manager] proxy stopped")
	return nil
}

func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil && m.cmd.Process != nil
}

func (m *Manager) PID() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil {
		return m.cmd.Process.Pid
	}
	return 0
}

func (m *Manager) RecentLog() []LogLine {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LogLine, len(m.ring))
	copy(out, m.ring)
	return out
}

func (m *Manager) AccessURLs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.accessURLs))
	copy(out, m.accessURLs)
	return out
}

func maskKey(k string) string {
	if len(k) <= 6 {
		return strings.Repeat("*", len(k))
	}
	return k[:3] + "****" + k[len(k)-3:]
}

// HasPassword reports whether a management password is configured.
func (m *Manager) HasPassword() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.TrimSpace(m.cfg.PasswordHash) != ""
}

// SetPassword hashes and stores a new management password (first-run setup).
func (m *Manager) SetPassword(pw string) error {
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg.PasswordHash = h
	m.mu.Unlock()
	m.SaveConfig()
	return nil
}

// CheckPassword verifies a candidate management password.
func (m *Manager) CheckPassword(pw string) bool {
	m.mu.Lock()
	h := m.cfg.PasswordHash
	m.mu.Unlock()
	return verifyPassword(h, pw)
}
