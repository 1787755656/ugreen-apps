package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
)

func loaderName() string {
	if runtime.GOARCH == "amd64" {
		return "ld-linux-x86-64.so.2"
	}
	return "ld-linux-aarch64.so.1"
}

// Supervisor 负责把 postgres 当子进程拉起来并守护。
//
// 为什么不用 pg_ctl：pg_ctl 会 fork 出 postmaster 然后自己退出，绿联只盯 start_cmd
// 那个进程，中间隔一层就没法准确反映状态、也不好转发信号。直接自己 fork+wait 最直白。
type Supervisor struct {
	app *App

	mu       sync.Mutex
	cmd      *exec.Cmd
	stopping bool          // 是不是我们主动要停（决定退出后要不要自动拉起）
	done     chan struct{} // 当前这条命令的 wait 结束信号
	phase    string        // 给管理页看的一句话状态
	lastErr  string
	started  time.Time
	fails    int // 连续"快速失败"次数
}

func newSupervisor(a *App) *Supervisor {
	return &Supervisor{app: a, phase: "尚未启动"}
}

func (s *Supervisor) setPhase(p string) {
	s.mu.Lock()
	s.phase = p
	s.mu.Unlock()
}

func (s *Supervisor) setLastErr(e string) {
	s.mu.Lock()
	s.lastErr = e
	s.mu.Unlock()
}

// Status 给管理页用的快照。
type Status struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid"`
	Phase   string `json:"phase"`
	LastErr string `json:"last_error"`
	Uptime  int64  `json:"uptime_sec"`
}

func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Phase: s.phase, LastErr: s.lastErr}
	if s.cmd != nil && s.cmd.Process != nil && s.cmd.ProcessState == nil {
		st.Running = true
		st.PID = s.cmd.Process.Pid
		st.Uptime = int64(time.Since(s.started).Seconds())
	}
	return st
}

// Start 拉起 postgres。已经在跑就什么都不做。
func (s *Supervisor) Start() error {
	s.mu.Lock()
	if s.cmd != nil && s.cmd.ProcessState == nil && s.cmd.Process != nil {
		s.mu.Unlock()
		return nil
	}
	s.stopping = false
	s.mu.Unlock()
	return s.spawn()
}

func (s *Supervisor) spawn() error {
	a := s.app
	logw, err := newRotatingLog(a.logFile, 4<<20)
	if err != nil {
		return fmt.Errorf("打开日志文件: %w", err)
	}

	// cwd 必须是 PGDATA：postgres 里的时区目录被改成了相对路径 ../zoneinfo。
	// postmaster 自己也会 ChangeToDataDir() 到这里，所以前后一致；
	// 而【它在 chdir 之前就要用到时区】（InitializeGUCOptions → pg_tzset("GMT")），
	// 所以我们必须一开始就把 cwd 设对，不能指望它自己 chdir。
	cmd := a.pgCommand(context.Background(), "postgres", a.PGData(), "-D", a.PGData())
	cmd.Stdout = logw
	cmd.Stderr = logw
	// 单独一个进程组：这样停止时可以一次性招呼到所有后台进程，
	// 也避免 launcher 收到的信号被内核直接转发给 postgres（我们要自己控制顺序和信号种类）。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logw.Close()
		s.mu.Lock()
		s.lastErr = err.Error()
		s.phase = "启动失败：" + err.Error()
		s.mu.Unlock()
		return err
	}

	done := make(chan struct{})
	s.mu.Lock()
	s.cmd = cmd
	s.done = done
	s.started = time.Now()
	s.phase = "正在启动…"
	s.lastErr = ""
	s.mu.Unlock()
	log.Printf("postgres 已启动，pid=%d", cmd.Process.Pid)

	go s.wait(cmd, logw, done)
	go s.probeReady()
	return nil
}

// probeReady 等数据库真正能接受连接，把状态从"正在启动"翻成"运行中"。
func (s *Supervisor) probeReady() {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ok := s.app.ready(ctx)
		cancel()
		if ok {
			s.mu.Lock()
			if s.cmd != nil && s.cmd.ProcessState == nil {
				s.phase = "运行中"
				s.fails = 0 // 真的跑起来过，崩溃计数清零
			}
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		dead := s.cmd == nil || s.cmd.ProcessState != nil
		s.mu.Unlock()
		if dead {
			return
		}
		time.Sleep(time.Second)
	}
	s.setPhase("启动超时：90 秒内没能接受连接，看日志")
}

func (s *Supervisor) wait(cmd *exec.Cmd, logw io.Closer, done chan struct{}) {
	err := cmd.Wait()
	logw.Close()
	close(done)

	s.mu.Lock()
	ran := time.Since(s.started)
	stopping := s.stopping
	if err != nil {
		s.lastErr = err.Error()
	}
	if stopping {
		s.phase = "已停止"
		s.mu.Unlock()
		log.Printf("postgres 已停止（%v）", err)
		return
	}
	// 意外退出。跑满 30 秒才退的不算"快速失败"——那更像是外部原因，
	// 无条件拉起即可；连续快速失败才说明是配置/环境问题，一直重试只会刷屏。
	if ran < 30*time.Second {
		s.fails++
	} else {
		s.fails = 0
	}
	fails := s.fails
	s.phase = fmt.Sprintf("意外退出（%v），准备重启", err)
	s.mu.Unlock()
	log.Printf("postgres 意外退出：%v（本次运行 %v，连续快速失败 %d 次）", err, ran.Round(time.Second), fails)

	if fails >= 5 {
		s.setPhase("连续 5 次启动即退，已放弃自动重启 —— 请看日志排查后手动启动")
		log.Printf("连续 5 次快速失败，停止自动重启")
		return
	}
	// 退避：1s, 2s, 4s, 8s, 16s
	backoff := time.Duration(1<<fails) * time.Second
	if backoff > 16*time.Second {
		backoff = 16 * time.Second
	}
	time.Sleep(backoff)

	s.mu.Lock()
	stopping = s.stopping
	s.mu.Unlock()
	if stopping {
		return
	}
	if err := s.spawn(); err != nil {
		log.Printf("重启失败: %v", err)
	}
}

// Stop 停止 postgres。
//
// 信号选择很重要，用错会丢数据或者停不下来：
//
//	SIGTERM  smart shutdown —— 等所有客户端自己断开，可能永远等下去，不适合"停止应用"
//	SIGINT   fast shutdown  —— 回滚进行中的事务、做一次检查点再退，这是我们要的
//	SIGQUIT  immediate      —— 直接退，下次启动要走崩溃恢复，只在实在停不掉时用
func (s *Supervisor) Stop() {
	s.mu.Lock()
	s.stopping = true
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil || done == nil {
		return
	}
	s.setPhase("正在停止…")

	_ = cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-done:
		return
	case <-time.After(25 * time.Second):
	}

	log.Printf("fast shutdown 25 秒未完成，改用 SIGQUIT（下次启动会走崩溃恢复）")
	_ = cmd.Process.Signal(syscall.SIGQUIT)
	select {
	case <-done:
		return
	case <-time.After(10 * time.Second):
	}

	log.Printf("SIGQUIT 也没停下来，只能 SIGKILL 整个进程组")
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = cmd.Process.Kill()
	}
	<-done
}

// Restart 停了再起。改端口、改访问范围之后要调它。
func (s *Supervisor) Restart() error {
	s.Stop()
	if err := s.app.writeConfFiles(); err != nil {
		return err
	}
	s.mu.Lock()
	s.stopping = false
	s.fails = 0
	s.mu.Unlock()
	return s.spawn()
}

// ---- 日志 ---------------------------------------------------------------

// rotatingLog 是一个带体积上限的日志文件。
// 超过上限就把当前文件挪成 .1 再开新的，最多留两代 —— 数据库日志在出问题时
// 涨得很快，不封顶会把应用数据目录撑爆。
type rotatingLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
	max  int64
	n    int64
}

func newRotatingLog(path string, max int64) (*rotatingLog, error) {
	if err := os.MkdirAll(fileDir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	var n int64
	if st != nil {
		n = st.Size()
	}
	return &rotatingLog{f: f, path: path, max: max, n: n}, nil
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return len(p), nil
	}
	if r.n+int64(len(p)) > r.max {
		r.f.Close()
		_ = os.Rename(r.path, r.path+".1")
		f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			r.f = nil
			return len(p), nil // 日志写不下去也绝不能影响数据库运行
		}
		r.f, r.n = f, 0
	}
	n, err := r.f.Write(p)
	r.n += int64(n)
	return n, err
}

func (r *rotatingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

func fileDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// Recorder 把 launcher 自己的日志同时写到 stderr 和一个环形缓冲，
// 好让管理页在"没有 SSH"的情况下也能看到启动过程里发生了什么。
type Recorder struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRecorder(max int) *Recorder { return &Recorder{max: max} }

func (r *Recorder) Write(p []byte) (int, error) {
	os.Stderr.Write(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ln := range splitLines(string(p)) {
		r.lines = append(r.lines, ln)
	}
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
	return len(p), nil
}

func (r *Recorder) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
