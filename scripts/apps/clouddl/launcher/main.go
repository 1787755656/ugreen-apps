package main

// 多网盘下载器 —— UGOS Pro 原生应用的管理壳。
//
// 上游：https://github.com/xiaocheng154/nas-cloud-downloader（作者 xiaocheng154，
// 采用要求署名的许可证）。本工程【没有改上游的业务代码】，适配全部集中在：
//
//	launcher/          这个 Go 管理壳（鉴权、守护进程、目录与环境）
//	service/src/ugos_main.py   一个只做"换个监听地址"的入口层
//
// 整体结构：
//
//	浏览器 ──HTTPS──> UGOS 网关 ──/api/*──> 管理壳(:28686) ──> Python(127.0.0.1:内部端口)
//	                     └──其余路径──> 网关自己 serve 的 www/（上游前端原样搬过来）

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
	downloadDir  string
	downloadNote string
	startedAt    time.Time
}

func main() {
	port := flag.Int("port", 28686, "管理壳监听的端口，必须和 project.yaml 的 port 一致")
	devNoAuth := flag.Bool("dev-no-auth", false, "本地开发用：跳过两道闸。绝不要在 NAS 上用")
	flag.Parse()

	// 日志直接打到 stdout —— 平台把它 append 到
	// /volume1/@appstore/<appid>/log/<appid>.log。journalctl 里【没有】这些内容。
	log.SetFlags(log.LstdFlags)
	log.Printf("[launcher] 多网盘下载器管理壳 %s 启动，端口 %d", version, *port)

	paths := resolvePaths()
	if err := paths.ensureDirs(); err != nil {
		log.Fatalf("[launcher] 建立数据目录失败：%v", err)
	}
	log.Printf("[launcher] 安装目录 %s，数据目录 %s", paths.Install, paths.Data)

	pruneStaleTemp(paths.Tmp)

	if err := ensureDefaultSettings(paths.Config); err != nil {
		// 不致命：上游自己也会在没有配置文件时用默认值跑起来
		log.Printf("[launcher] 写默认设置失败（不影响启动）：%v", err)
	}

	cfg := loadLauncherConfig(paths.LauncherConfig)
	decision := resolveDownloadDir(paths, &cfg)
	if decision.Note != "" {
		log.Printf("[launcher] 下载目录：%s", decision.Note)
	}
	log.Printf("[launcher] 本次使用的下载目录：%s", decision.Dir)

	internalPort, err := pickInternalPort(*port)
	if err != nil {
		log.Fatalf("[launcher] %v", err)
	}

	child := newSupervisor(paths, internalPort, decision.Dir)
	go child.Run()

	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(internalPort))
	proxy := httputil.NewSingleHostReverseProxy(target)
	// 下载进度是靠轮询的，但日志下载和诊断包导出是流式的大响应；
	// 不设 FlushInterval 的话会被缓冲住，界面上看着像卡死。
	proxy.FlushInterval = 200 * time.Millisecond

	srv := &Server{
		paths:        paths,
		child:        child,
		proxy:        proxy,
		sessions:     newSessionStore(),
		devNoAuth:    *devNoAuth,
		downloadDir:  decision.Dir,
		downloadNote: decision.Note,
		startedAt:    time.Now(),
	}
	proxy.ErrorHandler = srv.proxyError

	if *devNoAuth {
		log.Print("[launcher] ⚠ 已用 --dev-no-auth 关闭鉴权，仅限本地开发")
	}

	httpServer := &http.Server{
		Addr:    ":" + strconv.Itoa(*port),
		Handler: srv.routes(),
		// 下载大文件（日志包、缩略图）和长轮询都走这条，写超时不能太短；
		// 网关那边 proxy_read_timeout 是 3600s。
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

	// 用网关身份换会话 Cookie —— 让浏览器自己发起的 <img>/<a> 请求也能过闸。
	// 这个接口【只认网关身份】，拿已有 Cookie 换不出新 Cookie。
	// 注意它必须排在 /api/ 前面被精确匹配（Go 的 ServeMux 长前缀优先，这里是精确路径）。
	mux.HandleFunc("/api/session", s.requireGatewayOnly(s.handleSession))

	// 其余 /api/* 全部转给上游 Python。
	mux.HandleFunc("/api/", s.requireAuth(s.handleProxy))

	// 前端页面。生产环境走不到这里（网关自己从 www/ serve），
	// 只有本地开发和"有人直接敲端口"时才会命中 —— 后者要看到 403 整页。
	fs := http.FileServer(http.Dir(s.paths.WWW))
	mux.Handle("/", s.requirePageSameHost(fs))

	return mux
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	s.proxy.ServeHTTP(w, r)
}

// proxyError 把"上游没起来"翻译成人话。
//
// 默认的 502 页面对用户毫无价值，而这个应用最可能出问题的时刻恰恰是
// 刚装好、Python 还在起、或者起不来的时候。
func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	st := s.child.Status()
	msg := "下载服务还没准备好，请稍等几秒后重试。"
	if !st.Running {
		msg = "下载服务没有运行。若刚安装或刚升级，请在应用中心「停止 → 启动」一次；" +
			"仍不行的话，应用日志里 [python] 开头的那几行写了原因。"
		if st.LastExit != "" {
			msg += "（上次退出：" + st.LastExit + "）"
		}
	}
	log.Printf("[launcher] 反代 %s 失败：%v", r.URL.Path, err)
	writeErr(w, http.StatusServiceUnavailable, msg)
}
