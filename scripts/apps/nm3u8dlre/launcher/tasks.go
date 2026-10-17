package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxRunning = 3

// Task 是一个下载任务的持久化状态（tasks/<id>.json）。
// 与上游 index.cgi 的 .meta 等价，只是改用 JSON 表达。
type Task struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Name        string   `json:"name"`
	CreatedAt   string   `json:"createdAt"`
	StartedAt   string   `json:"startedAt,omitempty"`
	FinishedAt  string   `json:"finishedAt,omitempty"`
	Status      string   `json:"status"` // queued running stopping success failed stopped
	OutDir      string   `json:"outDir"`
	Auto        bool     `json:"auto"`
	SubOnly     bool     `json:"subOnly"`
	Live        bool     `json:"live"`
	Mux         string   `json:"mux"`
	Threads     int      `json:"threads"`
	Pid         int      `json:"pid"`
	ExitCode    *int     `json:"exitCode"`
	OutputFiles []string `json:"outputFiles"`
	RawParams   string   `json:"rawparams"`
	Engine      string   `json:"engine,omitempty"`
}

type taskStore struct {
	mu    sync.Mutex
	paths *Paths
	items map[string]*Task
	procs map[string]*os.Process
}

func newTaskStore(p *Paths) *taskStore {
	s := &taskStore{paths: p, items: map[string]*Task{}, procs: map[string]*os.Process{}}
	s.load()
	go s.reconcileLoop()
	return s
}

func (s *taskStore) metaPath(id string) string  { return filepath.Join(s.paths.Tasks, id+".json") }
func (s *taskStore) logPath(id string) string   { return filepath.Join(s.paths.Tasks, id+".log") }
func (s *taskStore) snapPath(id string) string  { return filepath.Join(s.paths.Tasks, id+".snapshot") }

func (s *taskStore) load() {
	ents, err := os.ReadDir(s.paths.Tasks)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		var t Task
		b, err := os.ReadFile(filepath.Join(s.paths.Tasks, e.Name()))
		if err != nil || json.Unmarshal(b, &t) != nil {
			continue
		}
		if t.ID == "" {
			t.ID = id
		}
		s.items[t.ID] = &t
	}
}

func (s *taskStore) save(t *Task) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp := s.metaPath(t.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath(t.ID))
}

func newID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano()%1e9, 10)
}

func nowISO() string { return time.Now().Format("2006-01-02T15:04:05-07:00") }

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// pid==1 之类绝不是我们启动的；沙箱里 /proc 只有 self，用 kill(0) 判活最稳
	if pid == 1 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// reconcileLoop 处理"应用重启后遗留的 running 任务"：
// 进程已经没了就把它标成 stopped，并补算产出文件。
func (s *taskStore) reconcileLoop() {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for range tick.C {
		s.mu.Lock()
		for _, t := range s.items {
			if t.ExitCode != nil {
				continue
			}
			if t.Status != "running" && t.Status != "stopping" && t.Status != "queued" {
				continue
			}
			if _, tracked := s.procs[t.ID]; tracked {
				continue
			}
			if pidAlive(t.Pid) {
				continue
			}
			// 没有跟踪的进程且 pid 已消失：收尾（不覆盖已有的成功/失败判定则用 stopped）
			if t.Status == "running" || t.Status == "stopping" {
				t.Status = "stopped"
				t.FinishedAt = nowISO()
				s.finishFiles(t)
				_ = s.save(t)
			}
		}
		s.mu.Unlock()
	}
}

// finishFiles 用"任务开始前快照"对目录做差集，得到本任务产出的文件。
// 与上游 task-run.sh 的做法一致：不扫描无关文件。
func (s *taskStore) finishFiles(t *Task) {
	snapRaw, err := os.ReadFile(s.snapPath(t.ID))
	if err != nil {
		return
	}
	var before []string
	if json.Unmarshal(snapRaw, &before) != nil {
		return
	}
	seen := map[string]bool{}
	for _, n := range before {
		seen[n] = true
	}
	ents, err := os.ReadDir(t.OutDir)
	if err != nil {
		return
	}
	var added []string
	for _, e := range ents {
		if e.IsDir() || seen[e.Name()] {
			continue
		}
		if strings.HasSuffix(e.Name(), ".log") || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		added = append(added, e.Name())
	}
	sort.Strings(added)
	t.OutputFiles = added
}

func snapshotDir(dir, snapFile string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		_ = os.WriteFile(snapFile, []byte("[]"), 0o644)
		return
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	b, _ := json.Marshal(names)
	_ = os.WriteFile(snapFile, b, 0o644)
}

// createArgs 把前端表单参数翻译成 N_m3u8DL-RE 命令行。
func createArgs(v map[string]string, outDir string, ffmpeg string) []string {
	var a []string
	get := func(k string) string { return strings.TrimSpace(v[k]) }
	on := func(k string) bool { return get(k) == "1" }

	if s := get("name"); s != "" {
		a = append(a, "--save-name", s)
	}
	if s := get("save_pattern"); s != "" {
		a = append(a, "--save-pattern", s)
	}
	if s := get("threads"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			a = append(a, "--thread-count", strconv.Itoa(n))
		}
	}
	if on("auto") {
		a = append(a, "--auto-select")
	}
	if on("concurrent") {
		a = append(a, "-mt")
	}
	if s := get("mux"); s != "" {
		a = append(a, "-M", "format="+s)
	}
	if on("subonly") {
		a = append(a, "--sub-only")
	}
	if s := get("sub_format"); s != "" {
		a = append(a, "--sub-format", s)
	}
	if on("live") {
		a = append(a, "--live-perform-as-vod")
	}
	if s := get("max_speed"); s != "" {
		a = append(a, "-R", s)
	}
	if s := get("tmp_dir"); s != "" {
		a = append(a, "--tmp-dir", s)
	}
	if s := get("retry_count"); s != "" {
		a = append(a, "--download-retry-count", s)
	}
	if s := get("timeout"); s != "" {
		a = append(a, "--http-request-timeout", s)
	}
	if s := get("custom_proxy"); s != "" {
		a = append(a, "--custom-proxy", s)
	}
	if get("use_system_proxy") == "0" {
		a = append(a, "--use-system-proxy", "false")
	}
	if s := get("headers"); s != "" {
		for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				a = append(a, "-H", line)
			}
		}
	}
	opt := func(key, flag string) {
		if s := get(key); s != "" {
			a = append(a, flag, s)
		}
	}
	opt("custom_range", "--custom-range")
	opt("task_start_at", "--task-start-at")
	opt("base_url", "--base-url")
	opt("select_video", "-sv")
	opt("select_audio", "-sa")
	opt("select_subtitle", "-ss")
	opt("drop_video", "-dv")
	opt("drop_audio", "-da")
	opt("drop_subtitle", "-ds")
	opt("ad_keyword", "--ad-keyword")
	opt("key", "--key")
	opt("key_text_file", "--key-text-file")
	opt("decryption_engine", "--decryption-engine")
	opt("decryption_binary_path", "--decryption-binary-path")
	opt("custom_hls_method", "--custom-hls-method")
	opt("custom_hls_key", "--custom-hls-key")
	opt("custom_hls_iv", "--custom-hls-iv")
	opt("log_level", "--log-level")
	opt("live_record_limit", "--live-record-limit")
	opt("live_wait_time", "--live-wait-time")

	for _, f := range []struct{ key, flag string }{
		{"skip_merge", "--skip-merge"},
		{"binary_merge", "--binary-merge"},
		{"append_url_params", "--append-url-params"},
		{"no_date_info", "--no-date-info"},
		{"disable_update_check", "--disable-update-check"},
		{"mp4_real_time_decryption", "--mp4-real-time-decryption"},
		{"no_log", "--no-log"},
		{"live_real_time_merge", "--live-real-time-merge"},
		{"live_pipe_mux", "--live-pipe-mux"},
	} {
		if on(f.key) {
			a = append(a, f.flag)
		}
	}
	if v, ok := v["live_keep_segments"]; ok && v == "0" {
		a = append(a, "--live-keep-segments", "false")
	}

	a = append(a, "--no-ansi-color")
	if ffmpeg != "" {
		a = append(a, "--ffmpeg-binary-path", ffmpeg)
	}
	a = append(a, "--save-dir", outDir)
	return a
}

var errBusy = errors.New("已有 3 个任务在运行，请稍后再试")

// Create 启动一个新任务。
func (s *taskStore) Create(vals map[string]string, rawQuery string) (*Task, error) {
	url := strings.TrimSpace(vals["url"])
	if url == "" {
		return nil, errors.New("下载链接不能为空")
	}
	if !(strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "/")) {
		return nil, errors.New("仅支持 http(s) 链接或本地文件路径")
	}

	outDir := strings.TrimSpace(vals["dir"])
	if outDir == "" || outDir == "null" {
		outDir = s.paths.Download
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("无法创建输出目录 %s: %v", outDir, err)
	}
	if !underAny(outDir, s.paths.allowedRoots()) {
		return nil, fmt.Errorf("输出目录不在已授权范围内（%s），请在应用设置里重新选择下载目录", outDir)
	}

	engine := s.paths.Engine()
	if _, err := os.Stat(engine); err != nil {
		return nil, fmt.Errorf("引擎不存在：%s", engine)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	running := 0
	for _, t := range s.items {
		if t.ExitCode == nil && pidAlive(t.Pid) {
			running++
		}
	}
	if running >= maxRunning {
		return nil, errBusy
	}

	id := newID()
	args := createArgs(vals, outDir, s.paths.FFmpeg)
	args = append(args, url)

	t := &Task{
		ID:        id,
		URL:       url,
		Name:      strings.TrimSpace(vals["name"]),
		CreatedAt: nowISO(),
		Status:    "queued",
		OutDir:    outDir,
		Auto:      vals["auto"] == "1",
		SubOnly:   vals["subonly"] == "1",
		Live:      vals["live"] == "1",
		Mux:       vals["mux"],
		Threads:   atoiOr(vals["threads"], 0),
		RawParams: stripAction(rawQuery),
		Engine:    engine,
	}
	if t.Name == "" {
		t.Name = id
	}

	logFile, err := os.OpenFile(s.logPath(id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("无法创建任务日志: %v", err)
	}

	snapshotDir(outDir, s.snapPath(id))

	cmd := exec.Command(engine, args...)
	cmd.Dir = outDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = engineEnv(s.paths, outDir)
	// 独立进程组：停止时对整组发信号，引擎派生的子进程（解密/混流工具）一并退出
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("启动引擎失败: %v", err)
	}
	logFile.Close()

	t.Pid = cmd.Process.Pid
	t.Status = "running"
	t.StartedAt = nowISO()
	s.items[id] = t
	s.procs[id] = cmd.Process
	_ = s.save(t)

	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
				if code < 0 {
					if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
						code = 128 + int(ws.Signal())
					}
				}
			} else {
				code = -1
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		t.ExitCode = &code
		t.FinishedAt = nowISO()
		switch {
		case t.Status == "stopping" || code >= 128:
			t.Status = "stopped"
		case code == 0:
			t.Status = "success"
		default:
			t.Status = "failed"
		}
		s.finishFiles(t)
		delete(s.procs, id)
		_ = s.save(t)
	}()

	return t, nil
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

// stripAction 去掉 action 参数，其余原始参数保留给"重试"用。
func stripAction(raw string) string {
	if raw == "" {
		return ""
	}
	out := make([]string, 0, 8)
	for _, kv := range strings.Split(raw, "&") {
		if kv == "" {
			continue
		}
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if k == "action" {
			continue
		}
		out = append(out, kv)
	}
	return strings.Join(out, "&")
}

// engineEnv 给引擎准备运行环境：沙箱无 /tmp、无 HOME，必须全部指向可写目录。
func engineEnv(p *Paths, outDir string) []string {
	env := os.Environ()
	env = append(env,
		"TMPDIR="+p.Tmp,
		"HOME="+p.Data,
		"DOTNET_BUNDLE_EXTRACT_BASE_DIR="+filepath.Join(p.Cache, "dotnet"),
	)
	return env
}

// resolve 返回任务当前视图（含实时进度）。
func (s *taskStore) status(id string) *Task {
	t := s.items[id]
	if t == nil {
		return nil
	}
	cp := *t
	alive := pidAlive(cp.Pid)
	switch {
	case cp.Status == "stopping" && !alive:
		cp.Status = "stopped"
	case alive && cp.ExitCode == nil:
		cp.Status = "running"
	case cp.ExitCode != nil:
		if *cp.ExitCode == 0 {
			cp.Status = "success"
		} else {
			cp.Status = "failed"
		}
	}
	return &cp
}

func (s *taskStore) list() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Task, 0, len(s.items))
	for _, t := range s.items {
		out = append(out, s.status(t.ID))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func (s *taskStore) Stop(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.items[id]
	if t == nil {
		return errors.New("任务不存在")
	}
	if !pidAlive(t.Pid) {
		return nil
	}
	t.Status = "stopping"
	_ = s.save(t)
	pid := t.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	go func() {
		time.Sleep(8 * time.Second)
		if pidAlive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	return nil
}

func (s *taskStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.items[id]
	if t == nil {
		return errors.New("任务不存在")
	}
	if pidAlive(t.Pid) {
		_ = syscall.Kill(-t.Pid, syscall.SIGKILL)
		_ = syscall.Kill(t.Pid, syscall.SIGKILL)
	}
	os.Remove(s.metaPath(id))
	os.Remove(s.logPath(id))
	os.Remove(s.snapPath(id))
	delete(s.items, id)
	delete(s.procs, id)
	return nil
}

// stopAll 在服务收到 SIGTERM 时调用：把仍在跑的任务进程组一并结束，
// 避免应用停止后遗留孤儿进程继续占带宽。
func (s *taskStore) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.items {
		if !pidAlive(t.Pid) {
			continue
		}
		_ = syscall.Kill(-t.Pid, syscall.SIGTERM)
		t.Status = "stopped"
		t.FinishedAt = nowISO()
		s.finishFiles(t)
		_ = s.save(t)
		delete(s.procs, id)
	}
}

// fileEntry 对应 /files 返回的单个文件。
type fileEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	RealPath string `json:"realPath"`
	Size     int64  `json:"size"`
	Mtime    string `json:"mtime"`
}

func (s *taskStore) Files() []fileEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []fileEntry{}
	for _, t := range s.items {
		if t.OutDir == "" || len(t.OutputFiles) == 0 {
			continue
		}
		for _, n := range t.OutputFiles {
			full := filepath.Join(t.OutDir, n)
			if seen[full] {
				continue
			}
			st, err := os.Stat(full)
			if err != nil || st.IsDir() {
				continue
			}
			seen[full] = true
			out = append(out, fileEntry{
				Name:     n,
				Path:     full,
				RealPath: realPath(full),
				Size:     st.Size(),
				Mtime:    st.ModTime().Format("2006-01-02 15:04:05"),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mtime > out[j].Mtime })
	return out
}

// taskOutDirs 用于把历史任务的输出目录也纳入允许访问的根。
func (s *taskStore) taskOutDirs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var dirs []string
	for _, t := range s.items {
		if t.OutDir != "" {
			dirs = append(dirs, t.OutDir)
		}
	}
	return dirs
}
