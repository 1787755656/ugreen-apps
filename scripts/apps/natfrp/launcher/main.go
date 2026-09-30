// Command launcher is a thin management shell around SakuraFrp's
// natfrp-service on UGOS Pro. It exists because:
//   - UGOS install/settings `parameters` are NOT injected as env vars on real
//     devices (verified: PARAM CHECK present=false), so the access key and Web
//     UI password can't be set that way.
//   - open_type: tab opens the port over http://, but natfrp's Web UI is
//     https-only, so the icon lands on a broken page.
//
// This launcher owns the natfrp-service subprocess and serves a tiny http
// management page on its own port (http is fine — the page has no WebSocket).
// From that page the user sets the access key / Web UI password (written into
// natfrp's config.json, then natfrp is restarted) and jumps to natfrp's real
// Web UI over the correct https:// URL. None of it depends on UGOS params.
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The management page. In open_type: inner the UGOS gateway serves this from
// rootfs_common/www/ (copied from this same file at build time) and proxies
// /api to us; embedding it too lets the launcher also serve it directly (e.g.
// tab mode or debugging). One source: tools/launcher/index.html.
//
//go:embed index.html
var indexHTML string

const natfrpWebUIPort = 7102

var (
	installDir string
	dataDir    string
	confPath   string
	natfrpBin  string
	frpcBin    string

	procMu   sync.Mutex
	natfrp   *exec.Cmd
	stopping bool
)

func main() {
	port := flag.Int("port", 7101, "http port for the management UI")
	flag.Parse()

	installDir = envOr("UGAPP_INSTALL_DIR", mustAbs(filepath.Dir(os.Args[0])+"/.."))
	dataDir = envOr("UGAPP_DATA_DIR", filepath.Join(installDir, "data"))
	_ = os.MkdirAll(dataDir, 0o755)
	confPath = filepath.Join(dataDir, "config.json")
	natfrpBin = filepath.Join(installDir, "bin", "natfrp-service")
	// frpc ships in the (read-only) install dir; it's updated by repackaging
	// the app with a newer binary. In-place update via natfrp's Web UI can't
	// work in the sandbox (it would need to overwrite the read-only launcher).
	frpcBin = filepath.Join(installDir, "bin", "frpc")

	ensureDefaults()
	startNatfrp()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/account", handleAccount)
	mux.HandleFunc("/api/dataplans", handleDataPlans)
	mux.HandleFunc("/api/tunnels", handleTunnels)
	mux.HandleFunc("/api/tunnel/autostart", handleTunnelAutostart)
	mux.HandleFunc("/api/remote", handleRemote)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: mux}
	go func() {
		log.Printf("[launcher] management UI on :%d (natfrp Web UI on https :%d)", *port, natfrpWebUIPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[launcher] http server failed: %v", err)
		}
	}()

	waitForSignal()
	log.Println("[launcher] shutting down…")
	stopNatfrp()
}

// --- config.json helpers ---

func readConfig() map[string]any {
	cfg := map[string]any{}
	if data, err := os.ReadFile(confPath); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	return cfg
}

func writeConfig(cfg map[string]any) error {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(confPath, out, 0o600)
}

// ensureDefaults seeds the minimal config so natfrp's Web UI always starts,
// without clobbering anything the user already has.
func ensureDefaults() {
	cfg := readConfig()
	setDefault(cfg, "webui_host", "0.0.0.0")
	setDefault(cfg, "webui_port", natfrpWebUIPort)
	setDefault(cfg, "webui_origin_mode", "any")
	setDefault(cfg, "update_interval", -1) // read-only install dir: self-update would fail
	setDefault(cfg, "log_stdout", true)
	setDefault(cfg, "webui_pass", "admin888")
	_ = writeConfig(cfg)
}

func setDefault(m map[string]any, k string, v any) {
	if _, ok := m[k]; !ok {
		m[k] = v
	}
}

// --- natfrp-service subprocess ---

func startNatfrp() {
	procMu.Lock()
	defer procMu.Unlock()
	spawnLocked()
}

func spawnLocked() {
	cmd := exec.Command(natfrpBin, "--daemon", "-c", confPath)
	cmd.Dir = dataDir
	cmd.Env = append(os.Environ(),
		"NATFRP_SERVICE_WD="+dataDir,
		"NATFRP_FRPC_PATH="+frpcBin,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("[launcher] failed to start natfrp-service: %v", err)
		return
	}
	natfrp = cmd
	log.Printf("[launcher] natfrp-service started (pid %d)", cmd.Process.Pid)
	go func() {
		err := cmd.Wait()
		procMu.Lock()
		defer procMu.Unlock()
		if stopping || natfrp != cmd {
			return // intentional stop or already replaced by a restart
		}
		log.Printf("[launcher] natfrp-service exited unexpectedly (%v), restarting in 3s", err)
		time.Sleep(3 * time.Second)
		if !stopping {
			spawnLocked()
		}
	}()
}

func stopNatfrp() {
	procMu.Lock()
	defer procMu.Unlock()
	stopping = true
	killLocked()
}

func killLocked() {
	if natfrp == nil || natfrp.Process == nil {
		return
	}
	_ = natfrp.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	cmd := natfrp
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		_ = natfrp.Process.Kill()
	}
	natfrp = nil
}

// restartNatfrp is used after a config change so natfrp reloads config.json.
func restartNatfrp() {
	procMu.Lock()
	defer procMu.Unlock()
	killLocked()
	spawnLocked()
}

func natfrpRunning() bool {
	procMu.Lock()
	defer procMu.Unlock()
	return natfrp != nil && natfrp.Process != nil
}

// --- HTTP handlers ---

func handleStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := readConfig()
	token, _ := cfg["token"].(string)
	pass, _ := cfg["webui_pass"].(string)
	remoteOn, _ := cfg["remote_management"].(bool)
	remoteKey, _ := cfg["remote_management_key"].(string)
	allowConfig, _ := cfg["remote_management_allow_config"].(bool)
	allowUpdate, _ := cfg["remote_management_allow_update"].(bool)
	writeJSON(w, map[string]any{
		"natfrp_running":      natfrpRunning(),
		"token_set":           token != "",
		"webui_pass_set":      pass != "" && pass != "admin888",
		"webui_port":          configWebUIPort(cfg), // follow natfrp's actual config, not a hardcoded value
		"webui_is_default":    pass == "" || pass == "admin888",
		"remote_enabled":      remoteOn,
		"remote_key_set":      remoteKey != "", // whether the E2E password has been set (in natfrp's Web UI)
		"remote_allow_config": allowConfig,
		"remote_allow_update": allowUpdate,
	})
}

// configWebUIPort reads the real webui_port from natfrp's config (JSON numbers
// decode to float64), falling back to the default if unset/invalid — so the
// "open Web UI" button always points at the port natfrp is actually serving.
func configWebUIPort(cfg map[string]any) int {
	if p, ok := cfg["webui_port"].(float64); ok && p > 0 {
		return int(p)
	}
	return natfrpWebUIPort
}

type configRequest struct {
	Token     *string `json:"token"`
	WebUIPass *string `json:"webui_pass"`
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req configRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不正确")
		return
	}

	cfg := readConfig()
	changed := false
	if req.Token != nil {
		cfg["token"] = strings.TrimSpace(*req.Token)
		changed = true
	}
	if req.WebUIPass != nil {
		p := strings.TrimSpace(*req.WebUIPass)
		if p != "" {
			if len(p) < 8 {
				writeErr(w, http.StatusBadRequest, "Web UI 密码至少 8 位")
				return
			}
			cfg["webui_pass"] = p
			changed = true
		}
	}

	if changed {
		if err := writeConfig(cfg); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存配置失败："+err.Error())
			return
		}
		restartNatfrp()
	}
	writeJSON(w, map[string]any{"ok": true})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch
}
