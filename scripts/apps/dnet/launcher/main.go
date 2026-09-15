// dnet 绿联原生应用管理壳。
//
// 职责：
//  1. 启动前把 dnet 的配置文件指到 UGAPP_DATA_DIR（沙箱安装目录只读，
//     上游默认写 os.UserHomeDir()，在沙箱里落到不可写的位置）
//  2. 修正 TMPDIR（沙箱没有 /tmp，上游自更新解包用 os.CreateTemp）
//  3. 守护包内自带的 dnet 二进制：意外退出自动重启（快退保护），
//     SIGTERM 转发、等子进程退出（平台 TimeoutStopSec=10s）
//  4. dnet 直接监听声明端口（19877），本壳不做反代——dnet 自带完整登录
//     认证（bcrypt + 失败锁定），真实客户端 IP 直达其 WAN 访问控制逻辑
//
// 注意：本壳 fork/wait 子进程，exec 的都是包内绝对路径，
// 不需要 SYSTEM.EXEC_SYSTEM_COMMAND 权限。
package main

import (
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// 与 project.yaml 的 port 必须一致（改一处必改另一处）
const declaredPort = 19877

const (
	fastExitLimit   = 30 * time.Second // 运行低于此时长算"快退"
	fastExitMax     = 5                // 连续快退次数上限
	restartInterval = 2 * time.Second
	stopGrace       = 6 * time.Second // SIGTERM 后等子进程退出的时间（平台总共给 10s）
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags)

	port := declaredPort
	if p := os.Getenv("UGOS_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
			port = n
		}
	}

	dataDir := envOr("UGAPP_DATA_DIR", ".")
	configPath := filepath.Join(dataDir, "dnet_config.yaml")

	// 沙箱没有 /tmp（TemporaryFileSystem=/:ro），os.TempDir() 会返回不可用的 /tmp
	if os.Getenv("TMPDIR") == "" {
		tmp := filepath.Join(envOr("UGAPP_CACHE_DIR", dataDir), "tmp")
		_ = os.MkdirAll(tmp, 0o755)
		_ = os.Setenv("TMPDIR", tmp)
	}

	exe := ""
	if p, err := os.Executable(); err == nil {
		exe = filepath.Dir(p)
	}
	dnetBin := filepath.Join(exe, "dnet")
	if _, err := os.Stat(dnetBin); err != nil {
		log.Fatalf("[launcher] 找不到 dnet 二进制 %s: %v", dnetBin, err)
	}

	// -noweb 不能加：WebUI 就是应用本体；-s/-u 不用（服务管理/自更新在沙箱里无意义）
	args := []string{"-l", ":" + strconv.Itoa(port), "-c", configPath}

	log.Printf("[launcher] 启动 dnet: %s %v", dnetBin, args)
	cmd := spawn(dnetBin, args)
	if cmd == nil {
		log.Fatalf("[launcher] dnet 启动失败")
	}
	startedAt := time.Now()

	// SIGTERM/SIGINT：转发给子进程，宽限后强杀
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, os.Interrupt)
	go func() {
		sig := <-sigCh
		log.Printf("[launcher] 收到 %v，停止 dnet", sig)
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		time.Sleep(stopGrace)
		if cmd.Process != nil {
			log.Printf("[launcher] dnet 未在 %v 内退出，强杀", stopGrace)
			_ = cmd.Process.Kill()
		}
		// 由 Wait 循环负责退出进程
	}()

	// 守护循环
	fastFails := 0
	for {
		err := cmd.Wait()
		state := cmd.ProcessState
		if err != nil {
			log.Printf("[launcher] dnet 退出: %v", err)
		} else {
			log.Printf("[launcher] dnet 正常退出")
		}

		// 正常退出 = 我们转发的停止信号被 dnet 处理了 → 整个应用按平台语义停止
		if state != nil && state.Success() {
			os.Exit(0)
		}

		if time.Since(startedAt) < fastExitLimit {
			fastFails++
			if fastFails >= fastExitMax {
				log.Printf("[launcher] 连续 %d 次快速崩溃，放弃拉起", fastFails)
				os.Exit(1)
			}
		} else {
			fastFails = 0
		}

		time.Sleep(restartInterval)
		log.Printf("[launcher] 重新拉起 dnet（连续快退 %d/%d）", fastFails, fastExitMax)
		cmd = spawn(dnetBin, args)
		if cmd == nil {
			log.Printf("[launcher] 重启失败，%v 后再试", restartInterval)
			time.Sleep(restartInterval)
			cmd = spawn(dnetBin, args)
			if cmd == nil {
				os.Exit(1)
			}
		}
		startedAt = time.Now()
	}
}

func spawn(bin string, args []string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("[launcher] 启动失败: %v", err)
		return nil
	}
	return cmd
}
