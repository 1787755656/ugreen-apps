package main

// 拉起并守护上游的 Flask 服务（Python）。
//
// 【为什么要有这层管理壳，不让 start_cmd 直接指向 Python】
//
//  1. 环境。沙箱里没有 /tmp、没有 /etc/passwd，安装目录只读；PYTHONHOME /
//     PYTHONPATH / TMPDIR / HOME 这些必须在 fork 之前摆好，而 start_cmd
//     不能带环境变量、也不经过 shell（沙箱里没有 /bin/sh）。
//  2. 目录。下载目录来自安装参数，还要处理"装完第一次启动参数是空的"。
//  3. 守护。Flask 崩了就地拉起来（带退避），不必等平台把整个应用判死。
//
// 【不需要 SYSTEM.EXEC_SYSTEM_COMMAND】：exec 的是包内自带的解释器
// （绝对路径、不经过 shell）。那条权限的全部作用是把 /bin /usr/bin 挂进沙箱，
// 我们一步都不碰。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type Supervisor struct {
	paths    Paths
	port     int    // Flask 监听的端口（只在 127.0.0.1 上）
	download string // 本次生效的下载目录

	mu        sync.Mutex
	running   bool
	starts    int
	lastExit  string
	lastStart time.Time

	cmd  *exec.Cmd
	stop chan struct{}
	done chan struct{}
}

func newSupervisor(paths Paths, port int, downloadDir string) *Supervisor {
	return &Supervisor{
		paths:    paths,
		port:     port,
		download: downloadDir,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// childEnv 组装交给 Python 的环境。
//
// 白名单式显式拼装：沙箱注入的 PATH / LD_LIBRARY_PATH / TZ 要留着，
// 其余全部由我们给，避免把不该进子进程的东西带进去。
func (s *Supervisor) childEnv() []string {
	env := []string{
		"PYTHONHOME=" + s.paths.PyHome,
		// vendor（第三方依赖）在前，service 根在后；后者其实由入口脚本自己
		// 塞进 sys.path，这里再给一次是免得将来换入口就崩。
		"PYTHONPATH=" + s.paths.Vendor + string(os.PathListSeparator) + s.paths.Service,
		// 安装目录只读，写不出 __pycache__。
		"PYTHONDONTWRITEBYTECODE=1",
		// 不设的话日志会攒在管道缓冲里，应用崩了反而看不到最后几行。
		"PYTHONUNBUFFERED=1",
		"PYTHONNOUSERSITE=1",
		"PYTHONIOENCODING=utf-8",

		// 上游只认这个：数据目录（数据库、session 密钥、日志都落这）。
		"APP_DATA_DIR=" + s.paths.Data,
		// 上游在「网关模式」才会把默认下载目录改到数据目录下；
		// 这里的路径由管理壳算好，由入口层落到 Setting 里。
		"UGOS_DOWNLOAD_DIR=" + s.download,
		// inner 应用的网关只反代 /api/*，整个站点就活在 /api 前缀下。
		"FNNAS_GATEWAY_PREFIX=/api",

		// ⚠ 沙箱里【没有 /tmp】。不重定向的话 tempfile.mkstemp 直接失败，
		//   而上游保存设置、原子写文件都靠它 —— 表现是"设置保存不了"。
		"TMPDIR=" + s.paths.Tmp,
		// 沙箱里没有 /etc/passwd，expanduser("~") 会失败。给个真实可写的家。
		"HOME=" + s.paths.Data,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	for _, key := range []string{"PATH", "LD_LIBRARY_PATH", "TZ"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
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

		// 跑满过 30 秒的退出当"偶发"，退避重置；一直起不来才拉长间隔，
		// 免得刷屏把日志盘写满。上限 30 秒是为了改完设置不用等太久。
		if ran > 30*time.Second {
			backoff = time.Second
		} else {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		log.Printf("[supervisor] Python 服务退出（%v，运行了 %s），%s 后重启",
			err, ran.Truncate(time.Second), backoff)

		select {
		case <-s.stop:
			return
		case <-time.After(backoff):
		}
	}
}

func (s *Supervisor) runOnce() error {
	if _, err := os.Stat(s.paths.Python); err != nil {
		return fmt.Errorf("找不到自带的 Python 解释器 %s：%w", s.paths.Python, err)
	}
	if _, err := os.Stat(s.paths.Entry); err != nil {
		return fmt.Errorf("找不到入口脚本 %s：%w", s.paths.Entry, err)
	}

	args := []string{s.paths.Entry, "--port", strconv.Itoa(s.port), "--data-dir", s.paths.Data}
	cmd := exec.Command(s.paths.Python, args...)
	cmd.Dir = s.paths.Data
	cmd.Env = s.childEnv()
	// 自成进程组：停应用时能把整棵子树（三个 API 子进程）一起带走。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 Python 失败：%w", err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.running = true
	s.starts++
	s.lastStart = time.Now()
	s.mu.Unlock()

	log.Printf("[supervisor] Python 服务已启动（pid %d，端口 %d，下载目录 %s）",
		cmd.Process.Pid, s.port, s.download)

	// 子进程输出转发到我们的 stdout —— 平台把它写到
	// /volume1/@appstore/<appid>/log/<appid>.log，那是排查启动失败唯一有用的地方
	// （journalctl 里只有 systemd 自己那句 Failed with result 'exit-code'）。
	forwardLines(stdout, "[python] ")

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
// 本函数最坏 5 + 1 秒，留有余量。
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
			log.Print("[supervisor] Python 没能在 5 秒内退出，强制结束")
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		}
	}

	select {
	case <-s.done:
	case <-time.After(time.Second):
	}
}

// pruneStaleTemp 清掉上次运行留在缓存目录里的过期临时文件。
// 缓存目录是持久的（不是 tmpfs），崩溃后残留会一直占着空间。
func pruneStaleTemp(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
