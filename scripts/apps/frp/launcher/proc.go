package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Svc 托管一个 frp 子进程(frpc/frps),单 goroutine 守护循环:
//   enabled==true  → 拉起,退出后按退避自动重启(运行满 3 分钟重置退避)
//   enabled==false → 停掉并挂起等待
// 状态变更统一通过 ch 信号唤醒,操作经 mutex 串行化。
type Svc struct {
	Name    string // "frpc" / "frps"
	Bin     string
	CfgPath string
	LogPath string

	mu       sync.Mutex
	enabled  bool
	cmd      *exec.Cmd
	runDone  chan struct{}
	started  time.Time
	lastErr  string // 最近一次异常退出原因(给状态页看)
	lastWait error  // 最近一次 cmd.Wait 的返回值
	pid      int

	ch chan struct{}
}

type SvcStatus struct {
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
	Pid     int    `json:"pid"`
	Since   string `json:"since,omitempty"`
	LastErr string `json:"last_err,omitempty"`
}

func newSvc(name string, st *store) *Svc {
	s := &Svc{
		Name:    name,
		Bin:     filepath.Join(binDir, name),
		CfgPath: filepath.Join(dataDir, name+".toml"),
		LogPath: filepath.Join(logDir, name+".log"),
		ch:      make(chan struct{}, 1),
	}
	s.enabled = (name == "frpc" && st.frpcEnabled()) || (name == "frps" && st.frpsEnabled())
	return s
}

func (s *Svc) signal() {
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

func (s *Svc) setEnabled(on bool) {
	s.mu.Lock()
	changed := s.enabled != on
	s.enabled = on
	s.mu.Unlock()
	if changed {
		log.Printf("[%s] 启用开关 → %v", s.Name, on)
		if !on {
			s.stopProc(6 * time.Second)
		}
		s.signal()
	}
}

func (s *Svc) getEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

// restart:进程活着就温和停掉,守护循环会立刻重新拉起(enabled 未变)
func (s *Svc) restart() {
	s.stopProc(6 * time.Second)
	s.signal()
}

// stopProc SIGTERM → 等退出 → 超时 SIGKILL。done 由守护循环在 Wait 返回后关闭。
func (s *Svc) stopProc(timeout time.Duration) {
	s.mu.Lock()
	cmd, done := s.cmd, s.runDone
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if done != nil {
		select {
		case <-done:
			return
		case <-time.After(timeout):
		}
	}
	_ = cmd.Process.Kill()
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}

func (s *Svc) snapshot() SvcStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SvcStatus{
		Enabled: s.enabled,
		Running: s.cmd != nil && s.pid > 0,
		Pid:     s.pid,
		LastErr: s.lastErr,
	}
	if st.Running {
		st.Since = s.started.Format("2006-01-02 15:04:05")
	}
	return st
}

func (s *Svc) setRunning(cmd *exec.Cmd, done chan struct{}) {
	s.mu.Lock()
	s.cmd = cmd
	s.runDone = done
	s.started = time.Now()
	s.pid = cmd.Process.Pid
	s.lastErr = ""
	s.mu.Unlock()
}

func (s *Svc) setStopped(lastErr string) {
	s.mu.Lock()
	s.cmd = nil
	s.runDone = nil
	s.pid = 0
	if lastErr != "" {
		s.lastErr = lastErr
	}
	s.mu.Unlock()
}

func (s *Svc) sleepOrSignal(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-s.ch:
		return false
	}
}

func (s *Svc) loop() {
	backoff := 2 * time.Second
	for {
		if !s.getEnabled() {
			<-s.ch
			backoff = 2 * time.Second
			continue
		}
		if err := s.launch(); err != nil {
			log.Printf("[%s] 启动失败: %v", s.Name, err)
			s.setStopped(err.Error())
			s.mgrLog("启动失败: " + err.Error())
			if !s.sleepOrSignal(backoff) {
				backoff = 2 * time.Second
				continue
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		s.mgrLog("进程已启动")
		s.wait()
		s.mu.Lock()
		waitErr := s.lastWait
		s.mu.Unlock()
		ran := s.ranFor()

		waitMsg := "<nil>"
		if waitErr != nil {
			waitMsg = waitErr.Error()
		}
		graceful := waitErr == nil ||
			strings.Contains(waitMsg, "signal: terminated") ||
			strings.Contains(waitMsg, "signal: killed")
		if graceful {
			s.setStopped("")
		} else {
			s.setStopped(waitMsg)
			s.mgrLog("异常退出: " + waitMsg)
		}
		log.Printf("[%s] 进程退出(%v),运行 %s", s.Name, waitMsg, ran.Truncate(time.Second))

		if !s.getEnabled() {
			backoff = 2 * time.Second
			continue
		}
		if ran > 3*time.Minute {
			backoff = 2 * time.Second
		}
		log.Printf("[%s] %s 后自动重启", s.Name, backoff)
		s.mgrLog(fmt.Sprintf("%s 后自动重启", backoff.Truncate(time.Second)))
		if !s.sleepOrSignal(backoff) {
			backoff = 2 * time.Second
			continue
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Svc) launch() error {
	s.mgrLog("--- 管理壳启动进程 ---")
	cmd := exec.Command(s.Bin, "-c", s.CfgPath)
	cmd.Env = ChildEnv()
	cmd.Dir = dataDir
	f, err := os.OpenFile(s.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("[%s] 打开日志文件失败: %v", s.Name, err)
	} else {
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		if f != nil {
			f.Close()
		}
		return err
	}
	done := make(chan struct{})
	go func() {
		if f != nil {
			defer f.Close()
		}
		waitErr := cmd.Wait()
		s.mu.Lock()
		s.lastWait = waitErr
		s.mu.Unlock()
		close(done)
	}()
	s.setRunning(cmd, done)
	return nil
}

// wait 阻塞至进程退出(launch 里的 goroutine 在 cmd.Wait 返回后关闭 runDone;
// 真实退出错误经 lastWait 字段带回)
func (s *Svc) wait() {
	s.mu.Lock()
	done := s.runDone
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (s *Svc) ranFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.started)
}

// mgrLog 把守护循环事件写进子进程日志,网页日志视图能看到生命周期
func (s *Svc) mgrLog(msg string) {
	line := fmt.Sprintf("[%s] [管理壳] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
	f, err := os.OpenFile(s.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// ChildEnv 固件会经 systemd DefaultEnvironment 注入 GODEBUG=tlskyber=0,
// go1.27+ 编译的二进制见到即 fatal;给子进程的 env 一律剥掉 GODEBUG,
// 并把 TMPDIR 指到沙箱内可写目录(沙箱没有 /tmp)。
func ChildEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GODEBUG=") || strings.HasPrefix(kv, "TMPDIR=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TMPDIR="+cacheTmp)
}

// verifyConfig 调 frp 自带的 verify 子命令校验配置;返回合并输出(截断)
func verifyConfig(svc string) (bool, string) {
	bin := filepath.Join(binDir, svc)
	cfg := filepath.Join(dataDir, svc+".toml")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "verify", "-c", cfg)
	cmd.Env = ChildEnv()
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > 4000 {
		text = text[:4000] + " ...(截断)"
	}
	if err != nil {
		return false, text
	}
	return true, text
}

// tailLines 读日志文件末尾 n 行(最多读 512KB)
func tailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	const maxRead = 512 << 10
	off := st.Size()
	if off > maxRead {
		off = maxRead
	}
	buf := make([]byte, off)
	if _, err := f.ReadAt(buf, st.Size()-off); err != nil {
		return nil
	}
	text := strings.TrimRight(string(buf), "\n")
	// 个别虚拟化文件系统对正被写入的文件会读出 NUL 垃圾段;日志视图里直接丢弃
	text = strings.ReplaceAll(text, "\x00", "")
	if strings.TrimSpace(text) == "" {
		return []string{}
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
