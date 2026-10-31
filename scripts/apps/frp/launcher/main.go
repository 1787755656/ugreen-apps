// FRP 内网穿透 UGOS 原生应用 —— Go 管理壳。
//
// 职责:网页登录、frps/frpc 配置文件编辑(带 frp 内置校验)、
// 双进程守护(自动重启/退避)、日志查看、管理密码管理。
// 纯标准库;frpc/frps 为上游官方二进制,由本壳 exec 托管。
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
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

//go:embed www/index.html www/icon.png
var wwwFS embed.FS

// 由 build.sh -ldflags -X 注入上游版本;完整包版本(upk 的 x.y.z.N)以
// 应用中心显示为准,管理壳只报上游版本号,不重复维护构建号。
var AppVersion = "0.71.1"

const defaultPort = 28600

var (
	dataDir    string
	logDir     string
	cacheDir   string
	installDir string
	binDir     string
	cacheTmp   string
)

func main() {
	port := flag.Int("port", defaultPort, "管理壳监听端口")
	showVer := flag.Bool("version", false, "打印版本并退出")
	flag.Parse()

	if *showVer {
		fmt.Printf("frp-mgr %s\n", AppVersion)
		return
	}

	resolveDirs()
	log.SetFlags(log.LstdFlags)
	log.Printf("frp-mgr v%s 启动 port=%d", AppVersion, *port)
	log.Printf("install=%s data=%s log=%s cache=%s", installDir, dataDir, logDir, cacheDir)

	st, plainPw, firstBoot := loadOrCreateStore(filepath.Join(dataDir, "manager.json"))
	if firstBoot {
		writeCredentialsFile(plainPw)
		log.Printf("首次启动:已生成管理密码与凭据说明.txt")
	}
	writeDefaultConfigs(firstBoot)

	frpc := newSvc("frpc", st)
	frps := newSvc("frps", st)
	frpVersion = detectFrpVersion()

	go frps.loop()
	go frpc.loop()
	// 读取持久化的启用状态并拉起
	frps.setEnabled(st.frpsEnabled())
	frpc.setEnabled(st.frpcEnabled())

	mux := http.NewServeMux()
	registerRoutes(mux, st, frpc, frps)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// SIGTERM:先温和停掉 frp 子进程再退出(平台停止窗口 10s)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigCh
		log.Printf("收到 %v,停止 frp 子进程并退出", s)
		frps.stopProc(4 * time.Second)
		frpc.stopProc(4 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("管理壳就绪: http://0.0.0.0:%d", *port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http 服务退出: %v", err)
	}
	log.Printf("frp-mgr 已退出")
}

func resolveDirs() {
	dataDir = envDir("UGAPP_DATA_DIR", "data")
	logDir = envDir("UGAPP_LOG_DIR", filepath.Join(dataDir, "log"))
	cacheDir = envDir("UGAPP_CACHE_DIR", filepath.Join(dataDir, "cache"))
	cacheTmp = filepath.Join(cacheDir, "tmp")
	for _, d := range []string{dataDir, logDir, cacheDir, cacheTmp} {
		mustMkdir(d)
	}
	// 沙箱没有 /tmp,本壳与子进程的临时目录统一指到 cache/tmp
	_ = os.Setenv("TMPDIR", cacheTmp)

	binDir = "bin"
	if self, err := os.Executable(); err == nil {
		binDir = filepath.Dir(self)
	}
	installDir = filepath.Dir(binDir)
}

func envDir(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func mustMkdir(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("创建目录失败 %s: %v", dir, err)
	}
}

var frpVersion = "unknown"

func detectFrpVersion() string {
	out, err := exec.Command(filepath.Join(binDir, "frpc"), "-v").Output()
	if err != nil {
		log.Printf("探测 frp 版本失败: %v", err)
		return "unknown"
	}
	return trimSpace(string(out))
}

// ---------- 默认配置模板 ----------

func writeDefaultConfigs(firstBoot bool) {
	// frps/frpc 模板共用同一个随机 token:出厂即可直连,用户再自行修改
	token := "please-change-me"
	if b, err := randomToken(24); err == nil {
		token = b
	}
	defaults := map[string]string{
		"frps.toml": defaultFrpsToml(token),
		"frpc.toml": defaultFrpcToml(token),
	}
	for name, content := range defaults {
		p := filepath.Join(dataDir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			log.Fatalf("探测配置文件失败 %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			log.Fatalf("写入默认配置失败 %s: %v", p, err)
		}
		log.Printf("已生成默认配置 %s", p)
	}
}

// frps 默认模板:管理壳首启时给 auth.token 填随机值,降低用户照抄默认弱密钥的概率
func defaultFrpsToml(token string) string {
	return `# ===== frp 服务端配置(frps)=====
# 其它机器上的 frpc 通过 bindPort 连入本 NAS。
bindPort = 7100

# 鉴权 token:客户端 frpc 的 auth.token 必须与此一致才能连入,务必修改!
auth.token = "` + token + `"

# 可选:服务端自带管理面板,启用后浏览器访问 http://<NAS_IP>:7500
#webServer.addr = "0.0.0.0"
#webServer.port = 7500
#webServer.user = "admin"
#webServer.password = "请修改"

# 可选:虚拟主机端口,frpc 的 http/https 类型代理需要
#vhostHTTPPort = 8080
#vhostHTTPSPort = 8443

# 可选:端口白名单,限制客户端 remotePort 可用范围
#allowPorts = [
#  { start = 6000, end = 6999 },
#]
`
}

func defaultFrpcToml(token string) string {
	return `# ===== frp 客户端配置(frpc)=====
# 连接远端 frp 服务端;serverPort 对应服务端 bindPort。
serverAddr = "127.0.0.1"
serverPort = 7100

# 与服务端 auth.token 保持一致
auth.token = "` + token + `"

# 示例:把 NAS 的 SSH(22) 映射到服务端 6022 端口
#[[proxies]]
#name = "ssh"
#type = "tcp"
#localIP = "127.0.0.1"
#localPort = 22
#remotePort = 6022

# 示例:通过服务端 vhostHTTPPort 按域名访问 NAS 网页服务
#[[proxies]]
#name = "web"
#type = "http"
#localPort = 80
#customDomains = ["nas.example.com"]

# 更多用法参考 https://gofrp.org/zh-cn/docs/
`
}

func trimSpace(s string) string { return strings.TrimSpace(s) }
