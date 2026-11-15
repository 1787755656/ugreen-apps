// librespeed-launcher —— LibreSpeed（speedtest-go）绿联 UGOS Pro 原生应用管理壳。
//
// 职责：
//  1. 前端资源同步（runtime-root 配方，坑 19）：安装树只读，自带前端（assets-ui，
//     speedtest.js/speedtest_worker.js LGPL 原件 + 中文 index.html）在启动时按版本戳
//     幂等同步到 data/www/；speedtest-go 的 assets_path 指向 data/www。用户改动
//     data/www/index.html 在同版本内保留，升级（版本戳变化）时整体刷新。
//  2. servername.json：每次启动按 SERVER_NAME 参数（应用中心「配置」里改，
//     停止→启动生效）重写 data/www/servername.json，前端读取后显示服务器名。
//  3. settings.toml 生成（仅首启，此后不覆盖——用户可改统计密码/坐标等业务键）：
//     监听端口/数据库文件/资源目录等基础设施值按 UGAPP 环境钉死生成。
//  4. 运行环境消毒（childEnv）：剥离 GODEBUG（固件注入 GODEBUG=tlskyber=0，
//     go1.27+ 二进制启动即 fatal；本版后端由 go1.26 编译天然免疫，剥离是升级
//     保险）、TZ/TZDIR 指包内 zoneinfo（坑 21，telemetry 时间戳要北京时间；后端
//     main.go 另有内嵌 time/tzdata 双保险）、TMPDIR 指 cache（沙箱无 /tmp）。
//  5. 守护 speedtest-backend：崩溃退避重启（稳定 10 分钟清零计数，连崩 5 次
//     exit(3) 交 systemd Restart=on-failure 兜底）；SIGTERM → TERM 子进程 →
//     10s 窗口（与平台一致）→ KILL。
//
// 端口面：单端口。后端直接绑 0.0.0.0:<port>（平台探活 + 浏览器直连 + 前端
// 同源自托管），launcher 不反代不占口——测速路径零代理开销，计量不失真。
//
// 用 go1.26 编译（GODEBUG 免疫 + 与固件注入无冲突）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

const (
	defaultPort     = 25340 // tab 面向端口（避开 25120/25121/25220/25230/25240/25250/25260/25280/25310/25320/25330/28888-28889）
	defaultServerNm = "绿联 NAS"
	statsPassword   = "LibreSpeed@2026" // /stats 历史页密码（上游无密码则页面拒绝出数据；介绍与凭据说明同步公开）
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	var (
		port       = flag.Int("port", defaultPort, "监听端口（平台探活 + 浏览器直连，写进生成的 settings.toml）")
		host       = flag.String("host", "0.0.0.0", "后端绑定地址（tab 需局域网可达）")
		dataFlag   = flag.String("data", "", "数据目录（默认 UGAPP_DATA_DIR，退安装目录/data）")
		installFlg = flag.String("install", "", "安装目录（默认 UGAPP_INSTALL_DIR，退可执行文件上两级）")
		backendFlg = flag.String("backend", "", "speedtest-backend 路径（默认 <install>/bin/speedtest-backend；Mac 冒烟可指 darwin 构建）")
		showVer    = flag.Bool("version", false, "打印版本并退出")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("librespeed-launcher", version)
		os.Exit(0)
	}

	installDir := *installFlg
	if installDir == "" {
		installDir = os.Getenv("UGAPP_INSTALL_DIR")
	}
	if installDir == "" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("定位安装目录失败: %v", err)
		}
		installDir = filepath.Dir(filepath.Dir(exe))
	}
	dataDir := *dataFlag
	if dataDir == "" {
		dataDir = os.Getenv("UGAPP_DATA_DIR")
	}
	if dataDir == "" {
		dataDir = filepath.Join(installDir, "data")
	}
	cacheDir := os.Getenv("UGAPP_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = filepath.Join(dataDir, "cache")
	}

	backendBin := *backendFlg
	if backendBin == "" {
		backendBin = filepath.Join(installDir, "bin", "speedtest-backend")
	}
	if st, err := os.Stat(backendBin); err != nil || st.IsDir() {
		log.Fatalf("后端二进制不存在: %s", backendBin)
	}
	assetsSrc := filepath.Join(installDir, "assets-ui")
	if st, err := os.Stat(filepath.Join(assetsSrc, "index.html")); err != nil || st.IsDir() {
		log.Fatalf("自带前端缺失: %s/index.html", assetsSrc)
	}
	zoneDir := filepath.Join(installDir, "zoneinfo")
	if _, err := os.Stat(zoneDir); err != nil {
		log.Printf("警告: 包内 zoneinfo 不存在（%s），时区回退后端内嵌 tzdata", zoneDir)
	}

	wwwDir := filepath.Join(dataDir, "www")
	settingsPath := filepath.Join(dataDir, "settings.toml")
	log.Printf("librespeed-launcher %s 启动: backend=%s data=%s www=%s port=%d",
		version, backendBin, dataDir, wwwDir, *port)

	for _, d := range []string{
		dataDir,
		filepath.Join(cacheDir, "tmp"),
		wwwDir,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("建目录失败 %s: %v", d, err)
		}
	}

	serverName := strings.TrimSpace(os.Getenv("SERVER_NAME"))
	if serverName == "" {
		serverName = defaultServerNm
	}
	syncWWW(assetsSrc, wwwDir, serverName)

	if err := ensureSettingsToml(settingsPath, *host, *port, wwwDir, dataDir); err != nil {
		log.Fatalf("生成 settings.toml 失败: %v", err)
	}
	writeUsageOnce(dataDir, *port)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	supervise(ctx, superviseOpts{
		backendBin:   backendBin,
		settingsPath: settingsPath,
		host:         *host,
		port:         *port,
		dataDir:      dataDir,
		cacheDir:     cacheDir,
		zoneDir:      zoneDir,
	})
}

// ---------- 前端同步（runtime-root 配方） ----------

// syncWWW 把安装树 assets-ui 同步到 data/www：版本戳（.version）不同则整体刷新
// （升级换新前端），相同则只保证 servername.json 与占位文件存在。用户对
// data/www 的改动在同版本内保留。
func syncWWW(src, dst, serverName string) {
	stamp := filepath.Join(dst, ".version")
	if b, err := os.ReadFile(stamp); err != nil || strings.TrimSpace(string(b)) != version {
		log.Printf("同步前端资源 %s -> %s（版本 %s）", src, dst, version)
		entries, err := os.ReadDir(src)
		if err != nil {
			log.Fatalf("读自带前端目录失败: %v", err)
		}
		for _, e := range entries {
			if e.Name() == ".version" {
				continue
			}
			s := filepath.Join(src, e.Name())
			d := filepath.Join(dst, e.Name())
			if err := copyPath(s, d); err != nil {
				log.Fatalf("同步前端文件 %s 失败: %v", e.Name(), err)
			}
		}
	} else {
		log.Printf("前端资源已同步（版本 %s），保留用户改动", version)
	}
	if err := os.WriteFile(stamp, []byte(version+"\n"), 0o644); err != nil {
		log.Fatalf("写版本戳失败: %v", err)
	}
	// servername.json 每次启动重写：SERVER_NAME 参数改动（停止→启动后）即时生效
	sj := fmt.Sprintf(`{"serverName":%q}`, serverName)
	if err := os.WriteFile(filepath.Join(dst, "servername.json"), []byte(sj), 0o644); err != nil {
		log.Fatalf("写 servername.json 失败: %v", err)
	}
	log.Printf("服务器名称: %s", serverName)
}

func copyPath(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		es, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range es {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm())
}

// ---------- 探活 ----------

// waitHealthy 探活：/ 任何 HTTP 响应都算后端就绪（200 静态页 / 404 资源缺失均代表端口已服务）。
func waitHealthy(o superviseOpts, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/", o.port)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
			resp.Body.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// spawnBackend 组装并启动 speedtest-backend 子进程（env 消毒见 childEnv）。
func spawnBackend(o superviseOpts) *exec.Cmd {
	cmd := exec.Command(o.backendBin, "-c", o.settingsPath)
	cmd.Dir = o.dataDir // cwd 指 data（可写），任何相对路径写盘不落安装树
	cmd.Env = childEnv(o)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("启动 speedtest-backend 失败: %v", err)
	}
	return cmd
}
