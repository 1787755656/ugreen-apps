// 100zip UGOS 移植 —— 管理壳。
//
// 上游：https://github.com/Alicace/100zip（MIT License，飞牛 fnOS 原生压缩/解压应用）。
// 本工程【不改上游业务代码】：上游 Go 源码直接交叉编译出 100zip 二进制，
// 所有沙箱适配都集中在这个管理壳里：
//
//	launcher/           本包（鉴权、会话 Cookie、反代、守护上游进程、授权目录注入）
//	scripts/build.sh    构建与打包（含断言）
//	scripts/patch-ui.py 前端断言式文案补丁
//	overlay/ugos-auth.js 前端认证适配层
//
// 整体结构：
//
//	浏览器 ──HTTPS──> UGOS 网关 ──/api/*──> 管理壳(:28737) ──> 上游(127.0.0.1:内部端口)
//	                     └──其余路径──> 网关自己 serve 的 www/（上游前端原样搬过来）

package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

var version = "dev"

type Server struct {
	paths        Paths
	child        *Supervisor
	proxy        *httputil.ReverseProxy
	sessions     *sessionStore
	devNoAuth    bool
	loginLimiter *loginRateLimiter
	loginKeys    *loginKeyStore
	ugosLogin    func(username, password string) error
	roots        []string
	rootsNote    string
	startedAt    time.Time
}

func main() {
	port := flag.Int("port", 28737, "管理壳监听的端口，必须和 project.yaml 的 port 一致")
	devNoAuth := flag.Bool("dev-no-auth", false, "本地开发用：跳过两道闸。绝不要在 NAS 上用")
	flag.Parse()

	log.SetFlags(log.LstdFlags)
	log.Printf("[launcher] 100解压管理壳 %s 启动，端口 %d", version, *port)

	paths := resolvePaths()
	if err := paths.ensureDirs(); err != nil {
		log.Fatalf("[launcher] 建立数据目录失败：%v", err)
	}
	log.Printf("[launcher] 安装目录 %s，数据目录 %s", paths.Install, paths.Data)

	roots, note := resolveRoots()
	if note != "" {
		log.Printf("[launcher] %s", note)
	}
	log.Printf("[launcher] 本次授权目录（%d 个）：%v", len(roots), roots)

	// 沙箱里没有 /tmp：上游包内预览用的 os.MkdirTemp("", "100zip-preview-")
	// 会直接失败（症状是"预览失败：创建临时目录"）。go 的 os.TempDir() 跟着
	// TMPDIR 走，fork 上游之前把它指到 cache 目录里。
	if err := os.MkdirAll(paths.Tmp, 0o755); err != nil {
		log.Fatalf("[launcher] 建立临时目录失败：%v", err)
	}

	internalPort, err := pickInternalPort(*port)
	if err != nil {
		log.Fatalf("[launcher] %v", err)
	}

	child := newSupervisor(paths, internalPort, roots)
	go child.Run()

	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(internalPort))
	proxy := httputil.NewSingleHostReverseProxy(target)
	// 大文件预览是流式响应（图片/视频/PDF 从压缩包里解出来直接转发），
	// 不设 FlushInterval 会被缓冲住，界面上看着像卡死。
	proxy.FlushInterval = 200 * time.Millisecond

	srv := &Server{
		paths:        paths,
		child:        child,
		proxy:        proxy,
		sessions:     newSessionStore(),
		loginLimiter: &loginRateLimiter{},
		loginKeys:    newLoginKeyStore(),
		ugosLogin:    newUgosLoginClient("https://127.0.0.1:9443").Login,
		devNoAuth:    *devNoAuth,
		roots:        roots,
		rootsNote:    note,
		startedAt:    time.Now(),
	}
	proxy.ErrorHandler = srv.proxyError

	if *devNoAuth {
		log.Print("[launcher] ⚠ 已用 --dev-no-auth 关闭鉴权，仅限本地开发")
	}

	httpServer := &http.Server{
		Addr:    ":" + strconv.Itoa(*port),
		Handler: srv.routes(),
		// 预览是长流式响应（视频在浏览器里边下边播）；网关那边
		// proxy_read_timeout 是 3600s，这里的写超时必须比一次预览更久。
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[launcher] 监听 %d 失败：%v（这个端口是不是被别的应用占了？）", *port, err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	log.Print("[launcher] 收到停止信号，正在关闭")

	// 先停对外的监听，再停子进程 —— 反过来的话，关闭期间进来的请求会打到
	// 一个正在死掉的上游上，用户看到的是莫名其妙的 502。
	//
	// ⚠ 时间预算：平台的 TimeoutStopSec 是 10 秒，超了直接 SIGKILL 整个 cgroup。
	//   这里 2 秒 + Supervisor.Stop 最多 6 秒 ≈ 8 秒，留了 2 秒余量。
	//   改任何一个数之前先看另一个。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	child.Stop()
	log.Print("[launcher] 已退出")
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// 探活：任何来源都放行，响应体里不含任何信息。
	// ⚠ 不能给它加闸 —— 平台的探测请求不经过网关、也可能不从预期地址来，
	//   挡了会让应用一直显示"启动失败"。
	mux.HandleFunc("/api/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})

	// 自查：不要求网关认证，但要求来自本机。鉴权链路不通时它是唯一的排查手段。
	mux.HandleFunc("/api/diag", s.requireSameHost(s.handleDiag))

	// 换会话 Cookie：桌面走网关身份（桥取 Ttk）；无桥环境（手机浏览器）
	// 走账号密码（管理壳到 UGOS 验密）。策略都在 handler 内部，见 login.go。
	mux.HandleFunc("/api/session", s.handleSession)

	// 登录加密通道的一次性公钥下发。压着本机闸：手机端全部流量经过网关
	// （源地址是 NAS 本机）不受影响，局域网上直连端口的请求拿不到钥匙。
	// 见 loginkeys.go。
	mux.HandleFunc("/api/session/key", s.requireSameHost(s.handleSessionKey))

	// 其余 /api/* 全部转给上游 100zip 服务。
	mux.HandleFunc("/api/", s.requireAuth(s.handleProxy))

	// 前端页面。生产环境走不到这里（网关自己从 www/ serve），
	// 只有本地开发和"有人直接敲端口"时才会命中 —— 后者要看到 403 整页。
	fs := http.FileServer(http.Dir(s.paths.WWW))
	mux.Handle("/", s.requirePageSameHost(fs))

	return mux
}

// handleProxy 把请求原样转给上游。鉴权已由 requireAuth 完成。
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[launcher] 反代到上游失败：%v", err)
	writeErr(w, http.StatusBadGateway, "压缩服务没有响应，可能正在启动或已退出。请稍候重试；若持续失败，请在应用中心停止再启动本应用。")
}
