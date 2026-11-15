package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type superviseOpts struct {
	backendBin   string
	settingsPath string
	host         string
	port         int
	dataDir      string
	cacheDir     string
	zoneDir      string
}

// childEnv 运行环境消毒：
//   - 剥离 GODEBUG：固件对全部 systemd 服务注入 GODEBUG=tlskyber=0，go1.27+ 编译的
//     二进制遇已移除键启动即 fatal（本版后端 go1.26 编译免疫；剥离是上游升工具链的保险）
//   - TZ/TZDIR：坑 21——沙箱无 /usr/share/zoneinfo，TZDIR 指包内 zoneinfo；后端
//     main.go 另 import 了 _ "time/tzdata"（内嵌 IANA 库）双保险，telemetry 时间戳
//     必须是北京时间
//   - TMPDIR：沙箱无 /tmp
//   - HOME：指 data（防任何库写 ~/.cache 落到不存在处）
//   - NO_PROXY：出站代理卫生（本版后端设计零出站，纯防御性；Mac 冒烟机器有系统代理）
func childEnv(o superviseOpts) []string {
	env := make([]string, 0, len(os.Environ())+8)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GODEBUG=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"TMPDIR="+filepath.Join(o.cacheDir, "tmp"),
		"TZ="+detectTimezone(),
		"TZDIR="+o.zoneDir,
		"HOME="+o.dataDir,
		"NO_PROXY=localhost,127.0.0.1,::1",
		"no_proxy=localhost,127.0.0.1,::1",
	)
	return env
}

// supervise 子进程守护：崩溃退避重启（5s×n 封顶 30s），稳定 10 分钟清零计数，
// 连崩 5 次 exit(3) 交 systemd Restart=on-failure 兜底；SIGTERM → TERM 子进程 →
// 10s 窗口（与平台一致）→ KILL。
func supervise(ctx context.Context, o superviseOpts) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		crashes := 0
		for {
			started := time.Now()
			cmd := spawnBackend(o)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()

			if waitHealthy(o, 60*time.Second) {
				log.Printf("speedtest-backend 就绪: http://%s:%d", o.host, o.port)
			} else {
				log.Printf("60s 未通过探活（进程仍在，继续观察）")
			}

			select {
			case <-ctx.Done():
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					_ = cmd.Process.Kill()
				}
				return
			case err := <-done:
				if time.Since(started) > 10*time.Minute {
					crashes = 0
				}
				crashes++
				log.Printf("speedtest-backend 退出（第 %d 次）: %v", crashes, err)
				if crashes >= 5 {
					log.Printf("连崩 %d 次，exit(3) 交 systemd 兜底", crashes)
					os.Exit(3)
				}
				sleep := time.Duration(crashes) * 5 * time.Second
				if sleep > 30*time.Second {
					sleep = 30 * time.Second
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(sleep):
				}
			}
		}
	}()
	wg.Wait()
	log.Printf("librespeed-launcher 退出")
}
