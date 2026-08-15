package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// 跑不满这么久就退出算"快退"，连续若干次快退才放弃。
	fastFailWindow = 30 * time.Second
	maxFastFails   = 5
	restartDelay   = 2 * time.Second

	// 平台停应用只等 10 秒就 SIGKILL 整个 cgroup。留点余量给自己收尾，
	// 剩下的全给 qBittorrent 存 resume 数据 —— 存不完下次就要重新校验。
	shutdownGrace = 8500 * time.Millisecond
)

type supervisor struct {
	bin  string
	args []string
	env  []string

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{}
	closed bool
}

func (s *supervisor) run() int {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logf("收到信号 %v，正在停止 qBittorrent…", sig)
		s.stop()
	}()

	fastFails := 0
	for {
		start := time.Now()
		err := s.once()
		if s.isClosed() {
			logf("qBittorrent 已退出，管理壳收工")
			return 0
		}
		lived := time.Since(start)

		var exitErr *exec.ExitError
		switch {
		case err == nil:
			logf("qBittorrent 正常退出（运行 %s），将重新拉起", lived.Round(time.Second))
		case errors.As(err, &exitErr):
			logf("qBittorrent 异常退出：%v（运行 %s）", exitErr, lived.Round(time.Second))
		default:
			logf("启动 qBittorrent 失败：%v", err)
		}

		if lived < fastFailWindow {
			fastFails++
			if fastFails >= maxFastFails {
				logf("连续 %d 次启动后很快退出，不再重试。原因请看上面 [qbittorrent] 开头的输出；"+
					"最常见的是 WebUI 端口被占用，或配置文件损坏", fastFails)
				return 1
			}
		} else {
			fastFails = 0
		}
		time.Sleep(restartDelay)
	}
}

func (s *supervisor) once() error {
	// 自己开管道而不用 StdoutPipe：stdout/stderr 要合流到同一条，
	// 且 Wait() 与管道关闭的先后关系自己掌握更省心。
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(s.bin, s.args...)
	cmd.Env = s.env
	cmd.Stdout = pw
	cmd.Stderr = pw

	exited := make(chan struct{})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		pr.Close()
		pw.Close()
		return nil
	}
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		pr.Close()
		pw.Close()
		return err
	}
	s.cmd, s.exited = cmd, exited
	s.mu.Unlock()

	pw.Close() // 父进程这份必须关掉，否则读端永远等不到 EOF
	logf("qBittorrent 已启动 (pid %d)", cmd.Process.Pid)

	pipeLines(pr)
	pr.Close()
	waitErr := cmd.Wait()
	close(exited)
	return waitErr
}

// pipeLines 把子进程输出逐行转发到自己的 stdout（也就是应用日志文件），
// 顺手加个前缀，好和管理壳自己的日志区分开。
func pipeLines(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		os.Stdout.WriteString("[qbittorrent] " + line + "\n")
	}
}

func (s *supervisor) stop() {
	s.mu.Lock()
	s.closed = true
	cmd, exited := s.cmd, s.exited
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return
	}
	select {
	case <-exited:
		logf("qBittorrent 已优雅退出")
	case <-time.After(shutdownGrace):
		logf("qBittorrent 在 %s 内没退出，强制结束", shutdownGrace)
		_ = cmd.Process.Kill()
	}
}

func (s *supervisor) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
