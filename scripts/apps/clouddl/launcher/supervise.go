package main

// 拉起并守护上游的 Python 服务。
//
// 【为什么要有这层管理壳，不让 start_cmd 直接指向 Python】
//
//  1. 鉴权。上游没有任何登录，而 inner 应用的声明端口在局域网上是直接可达的
//     （真机实测），等于把别人的网盘账号开放给整个局域网。两道闸做在这里。
//  2. 上游写死了 uvicorn.run(host="0.0.0.0")。管理壳让它只听 127.0.0.1，
//     外面那一层由我们把关。
//  3. 环境。沙箱里没有 /tmp、没有 /etc/passwd、安装目录只读，
//     PYTHONHOME/TMPDIR/HOME 这些都得在 fork 之前摆好。
//  4. 目录。下载目录来自安装参数，还要处理"首次启动参数是空的"那个平台行为。
//
// 【不需要 SYSTEM.EXEC_SYSTEM_COMMAND】：exec 的是包内自带的解释器，绝对路径、
// 不经过 shell。那条权限的全部作用是把 /bin /usr/bin 挂进沙箱，我们一步都不碰。

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
	paths    Paths
	port     int // Python 监听的内部端口（只在 127.0.0.1 上）
	download string

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
// 刻意【不】继承整个环境：沙箱注入的 LD_LIBRARY_PATH 等要留着，
// 但网盘凭据之类不该出现在子进程里的东西也没有，所以这里是白名单式的显式拼装
// （PATH/LD_LIBRARY_PATH 从父进程带过来，其余全是我们自己给的）。
func (s *Supervisor) childEnv() []string {
	env := []string{
		"PYTHONHOME=" + s.paths.PyHome,
		// vendor 在前、src 在后；src 其实由脚本自身所在目录进 sys.path[0]，
		// 这里再显式给一次，免得将来入口换个位置就崩。
		"PYTHONPATH=" + s.paths.Vendor + string(os.PathListSeparator) + s.paths.Src,
		// 安装目录只读，写不出 __pycache__。不关掉的话每次导入都白试一遍。
		"PYTHONDONTWRITEBYTECODE=1",
		// 不设的话日志会攒在管道缓冲里，应用崩了反而看不到最后几行。
		"PYTHONUNBUFFERED=1",

		"CONFIG_DIR=" + s.paths.Config,
		"DOWNLOAD_DIR=" + s.download,
		"DOWNLOAD_DIR_FILE=" + s.paths.DownloadDirFile,
		"PORT=" + strconv.Itoa(s.port),

		// ⚠ 沙箱里【没有 /tmp】。不重定向的话 tempfile.mkstemp 直接失败，
		//   而上游的原子写配置就是靠它 —— 表现会是"设置保存不了"。
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

		// 跑满过 30 秒的退出当"偶发"，退避重置；一直起不来才逐步拉长间隔，
		// 免得刷屏刷满日志盘。上限 30 秒是为了让用户改完设置后不用等太久。
		if ran > 30*time.Second {
			backoff = time.Second
		} else {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		log.Printf("[supervisor] Python 服务退出（%v，运行了 %s），%s 后重启", err, ran.Truncate(time.Second), backoff)

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

	cmd := exec.Command(s.paths.Python, s.paths.Entry)
	cmd.Dir = s.paths.Data
	cmd.Env = s.childEnv()
	// 自成进程组：停应用时能把整棵子树一起带走，
	// 而不是只杀掉解释器、留下它 fork 的东西。
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

	log.Printf("[supervisor] Python 服务已启动（pid %d，内部端口 %d，下载目录 %s）",
		cmd.Process.Pid, s.port, s.download)

	// 子进程的输出转发到我们的 stdout —— 平台把 stdout 重定向到
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
			log.Print("[supervisor] Python 没能在 5 秒内退出，强制结束")
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

// pickInternalPort 选一个空闲的 loopback 端口给 Python 用。
//
// 从 declared+1 开始试。为什么不直接写死：沙箱和宿主共用 loopback，
// 写死的端口可能被 NAS 上别的应用占着，而那种失败很难看出来
// （上游在 bind 之前就打了"服务已启动"的日志）。
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

// pruneStaleTemp 清掉上次运行留在缓存目录里的临时文件。
// 缓存目录是持久的（不是 tmpfs），崩溃后残留的分片会一直占着空间。
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
