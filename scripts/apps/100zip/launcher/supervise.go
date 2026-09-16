package main

// 拉起并守护上游的 100zip 服务。
//
// 【为什么要有这层管理壳，不让 start_cmd 直接指向 100zip】
//
//  1. 鉴权。上游没有任何登录，而 inner 应用的声明端口在局域网上是直接可达的
//     （真机实测），等于把 NAS 压缩包和密码库开放给整个局域网。两道闸做在这里。
//  2. 授权目录注入。上游靠 --allow-root 参数和 TRIM_* 环境变量拿授权根，
//     管理壳把 UGOS 授权的目录翻译成 --allow-root 再 fork。
//  3. 环境。沙箱里没有 /tmp，上游预览用的 os.MkdirTemp 会失败；
//     TMPDIR 要在 fork 之前摆好。
//
// 【不需要 SYSTEM.EXEC_SYSTEM_COMMAND】：exec 的是包内自带的二进制，
// 绝对路径、不经过 shell。沙箱没有 seccomp，execve 本身不受限。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type Supervisor struct {
	paths Paths
	port  int    // 上游监听的内部端口（只在 127.0.0.1 上）
	roots []string

	mu        sync.Mutex
	running   bool
	starts    int
	lastExit  string
	lastStart time.Time

	cmd  *exec.Cmd
	stop chan struct{}
	done chan struct{}
}

func newSupervisor(paths Paths, port int, roots []string) *Supervisor {
	return &Supervisor{
		paths: paths,
		port:  port,
		roots: roots,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// childEnv 组装交给上游的环境。
//
// 上游对环境的要求很少（dataDir/www/engine 都走命令行参数），
// 这里只需要补沙箱缺的东西。
func (s *Supervisor) childEnv() []string {
	env := []string{
		// ⚠ 沙箱里【没有 /tmp】。上游包内预览用 os.MkdirTemp("", "100zip-preview-")，
		//   go 的 os.TempDir() 跟着 TMPDIR 走 —— 不重定向的话预览全部失败。
		"TMPDIR=" + s.paths.Tmp,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	for _, key := range []string{"PATH", "TZ"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// args 组装上游的启动参数。上游 server/main.go 支持的 flag：
//
//	--addr 127.0.0.1:PORT   TCP 监听（上游强制只允许 loopback，正合适）
//	--data DIR              数据目录（任务记录、密码库、偏好）
//	--www DIR               前端静态目录（网关 serve 不到时兜底）
//	--engine PATH           7zzs 绝对路径
//	--allow-root PATH       授权根（可重复）
func (s *Supervisor) args() []string {
	args := []string{
		"--addr", "127.0.0.1:" + strconv.Itoa(s.port),
		"--data", s.paths.Data,
		"--www", s.paths.WWW,
		"--engine", s.paths.Engine,
	}
	for _, r := range s.roots {
		args = append(args, "--allow-root", r)
	}
	return args
}

// Run 一直守着子进程，直到 Stop 被调用。
func (s *Supervisor) Run() {
	defer close(s.done)

	backoff := time.Second
	for {
		select {
		case <-s.stop:
			return
		default:
		}

		start := time.Now()
		err := s.runOnce()
		ran := time.Since(start)

		select {
		case <-s.stop:
			return
		default:
		}

		s.mu.Lock()
		s.running = false
		if err != nil {
			s.lastExit = err.Error()
		} else {
			s.lastExit = "正常退出"
		}
		s.mu.Unlock()

		// 跑满过 30 秒的退出当"偶发"，退避重置；一直起不来才逐步拉长间隔。
		// 上限 30 秒是为了让用户改完设置后不用等太久。
		if ran > 30*time.Second {
			backoff = time.Second
		} else {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		log.Printf("[supervisor] 100zip 服务退出（%v，运行了 %s），%s 后重启", err, ran.Truncate(time.Second), backoff)

		select {
		case <-s.stop:
			return
		case <-time.After(backoff):
		}
	}
}

func (s *Supervisor) runOnce() error {
	if _, err := os.Stat(s.paths.Bin); err != nil {
		return fmt.Errorf("找不到上游二进制 %s：%w", s.paths.Bin, err)
	}
	if _, err := os.Stat(s.paths.Engine); err != nil {
		return fmt.Errorf("找不到 7-Zip 引擎 %s：%w", s.paths.Engine, err)
	}

	cmd := exec.Command(s.paths.Bin, s.args()...)
	cmd.Dir = s.paths.Data
	cmd.Env = s.childEnv()
	// 自成进程组：停应用时能把整棵子树一起带走（7zzs 是它的子进程）。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 100zip 失败：%w", err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.running = true
	s.starts++
	s.lastStart = time.Now()
	s.mu.Unlock()

	log.Printf("[supervisor] 100zip 服务已启动（pid %d，内部端口 %d，引擎 %s）",
		cmd.Process.Pid, s.port, filepath.Base(s.paths.Engine))

	// 子进程的输出转发到我们的 stdout —— 平台把 stdout 重定向到
	// /volume1/@appstore/<appid>/log/<appid>.log，那是排查启动失败唯一有用的地方。
	forwardLines(stdout, "[100zip] ")

	return cmd.Wait()
}

func forwardLines(r io.Reader, prefix string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	go func() {
		for scanner.Scan() {
			log.Print(prefix, scanner.Text())
		}
	}()
}

// Stop 优雅停掉子进程。
//
// 平台的 TimeoutStopSec 是 10 秒，超时会 SIGKILL 整个 cgroup。
// 本函数最坏 5 + 1 = 6 秒，加上 main 里 HTTP 关闭的 2 秒还有余量。
func (s *Supervisor) Stop() {
	close(s.stop)

	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		pgid := -cmd.Process.Pid
		_ = syscall.Kill(pgid, syscall.SIGTERM)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		exited := make(chan struct{})
		go func() {
			for {
				if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
					close(exited)
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
		select {
		case <-exited:
		case <-ctx.Done():
			log.Print("[supervisor] 100zip 没能在 5 秒内退出，强制结束（正在写的压缩任务会被打断）")
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		}
	}

	select {
	case <-s.done:
	case <-time.After(time.Second):
	}
}

type supervisorStatus struct {
	Running  bool   `json:"running"`
	Starts   int    `json:"starts"`
	LastExit string `json:"last_exit,omitempty"`
	Uptime   string `json:"uptime,omitempty"`
}

func (s *Supervisor) Status() supervisorStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := supervisorStatus{Running: s.running, Starts: s.starts, LastExit: s.lastExit}
	if s.running {
		st.Uptime = time.Since(s.lastStart).Truncate(time.Second).String()
	}
	return st
}

// pickInternalPort 选一个空闲的 loopback 端口给上游用。
//
// 从 declared+1 开始试。为什么不直接写死：沙箱和宿主共用 loopback，
// 写死的端口可能被 NAS 上别的应用占着，而那种失败很难看出来
// （上游在 bind 失败前已经打出了"ready"日志）。
func pickInternalPort(base int) (int, error) {
	for port := base + 1; port < base+20; port++ {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("在 %d-%d 之间找不到空闲的内部端口", base+1, base+19)
}
