// Command randomvideo_serv 是「随机视频」绿联 UGOS Pro 原生应用的后端。
//
// 职责：
//  1. serve 静态前端（编译期内嵌，见 embed.go）
//  2. /api/next     —— 向 hopolcn 的随机视频接口取一个新视频，登记后返回句柄 id
//  3. /api/stream   —— 按句柄 id 代理视频流（支持 Range），绕开跨域/防盗链/客户端直连受限
//  4. /api/raw      —— 按句柄 id 302 到源站，供前端「直连模式」使用
//  5. /api/heartbeat —— 平台健康检查
//
// 关键安全设计：浏览器永远拿不到「代理任意 URL」的能力。句柄只由服务端自己
// 从上游 302 跟随出来，且必须是通过 host 后缀白名单校验的 https 地址。
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// upstreamAPI 是 hopolcn 首页右侧视频框实际请求的接口：每次 GET 返回 302 到一个随机视频。
	// ⚠ 实测：不带 Referer 时它永远返回同一个默认视频，必须带 Referer 才会轮换。
	upstreamAPI = "https://api.hopolcn.com/api"
	upstreamRef = "https://www.hopolcn.com/"

	appVersion = "0.1.0"

	// queueDepth 预取队列深度：刷得越快，队首越可能已经在等着了。
	queueDepth = 4

	// handleTTL 句柄有效期。CDN URL 实际长期有效，这里只是给内存表兜底。
	handleTTL = 30 * time.Minute
	// handleMax 句柄表上限，超出后按登记顺序淘汰最旧的。
	handleMax = 512

	fetchTimeout = 15 * time.Second
	proxyTimeout = 10 * time.Minute
)

// allowedHostSuffixes 放行名单：上游回源到的 CDN 域名后缀。
// 实测见到的是 txmov2.a.kwimgs.com / alimov2.a.kwimgs.com 这类。
// 将来若换 CDN，用环境变量 RV_EXTRA_HOSTS 追加（逗号分隔）即可，不用重新打包。
var allowedHostSuffixes = []string{
	"kwimgs.com",
	"kwaicdn.com",
	"kuaishou.com",
	"kwai.com",
	"gifshow.com",
	"ksyungslb.com",
}

func extraHosts() []string {
	raw := os.Getenv("RV_EXTRA_HOSTS")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func hostAllowed(host string) bool {
	host = strings.ToLower(host)
	if host == "" {
		return false
	}
	candidates := allowedHostSuffixes
	candidates = append(append([]string{}, candidates...), extraHosts()...)
	for _, suf := range candidates {
		if host == suf || strings.HasSuffix(host, "."+suf) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 句柄表

type handle struct {
	URL   string
	Added time.Time
}

type registry struct {
	mu      sync.Mutex
	items   map[string]handle
	order   []string // 登记顺序，用于 FIFO 淘汰
	counter uint64
}

func newRegistry() *registry {
	return &registry{items: make(map[string]handle)}
}

func (r *registry) put(rawURL string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		sum := sha256.Sum256([]byte(rawURL + time.Now().String()))
		copy(b[:], sum[:16])
	}
	id := hex.EncodeToString(b[:])

	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[id] = handle{URL: rawURL, Added: time.Now()}
	r.order = append(r.order, id)
	r.counter++
	for len(r.order) > handleMax {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.items, oldest)
	}
	return id
}

func (r *registry) get(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.items[id]
	if !ok {
		return "", false
	}
	if time.Since(h.Added) > handleTTL {
		delete(r.items, id)
		return "", false
	}
	return h.URL, true
}

func (r *registry) served() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counter
}

// ---------------------------------------------------------------- 上游抓取

type upstreamError struct{ msg string }

func (e *upstreamError) Error() string { return e.msg }

const uaForUpstream = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// fetchRandomVideo 向接口请求一个新视频，返回源站 URL。
// 接口是 302，这里刻意不跟随重定向，只取 Location。
func fetchRandomVideo(client *http.Client) (string, error) {
	req, err := http.NewRequest(http.MethodGet, upstreamAPI+"?r="+strconv.FormatInt(time.Now().UnixNano(), 10), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Referer", upstreamRef)
	req.Header.Set("User-Agent", uaForUpstream)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch resp.StatusCode {
	case http.StatusFound, http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return "", &upstreamError{fmt.Sprintf("接口返回异常状态码 %d", resp.StatusCode)}
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", &upstreamError{"接口没有返回跳转地址"}
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", &upstreamError{"接口返回的跳转地址无法解析"}
	}
	if u.Scheme != "https" {
		return "", &upstreamError{"视频地址不是 https，已拒绝"}
	}
	if !hostAllowed(u.Hostname()) {
		return "", &upstreamError{fmt.Sprintf("视频域名 %s 不在放行名单内", u.Hostname())}
	}
	return loc, nil
}

// ---------------------------------------------------------------- 预取队列

type prefetcher struct {
	reg    *registry
	client *http.Client
	ch     chan string // 已登记好的句柄
	stop   chan struct{}
	once   sync.Once
	log    *log.Logger
}

func newPrefetcher(reg *registry, lg *log.Logger) *prefetcher {
	p := &prefetcher{
		reg:    reg,
		client: fetchClient(),
		ch:     make(chan string, queueDepth),
		stop:   make(chan struct{}),
		log:    lg,
	}
	go p.loop()
	return p
}

func (p *prefetcher) loop() {
	for {
		if !p.sleep(300 * time.Millisecond) {
			return
		}
		if len(p.ch) >= queueDepth {
			if !p.sleep(500 * time.Millisecond) {
				return
			}
			continue
		}
		raw, err := fetchRandomVideo(p.client)
		if err != nil {
			p.log.Printf("预取失败: %v", err)
			if !p.sleep(5 * time.Second) {
				return
			}
			continue
		}
		id := p.reg.put(raw)
		select {
		case p.ch <- id:
		case <-p.stop:
			return
		}
	}
}

// sleep 期间可被 stop 打断；返回 false 表示应该退出循环。
func (p *prefetcher) sleep(d time.Duration) bool {
	select {
	case <-p.stop:
		return false
	case <-time.After(d):
		return true
	}
}

// take 取一个句柄：优先用预取好的，没有就现抓。
func (p *prefetcher) take() (string, error) {
	select {
	case id := <-p.ch:
		return id, nil
	default:
	}
	raw, err := fetchRandomVideo(p.client)
	if err != nil {
		return "", err
	}
	return p.reg.put(raw), nil
}

func (p *prefetcher) close() { p.once.Do(func() { close(p.stop) }) }

// ---------------------------------------------------------------- HTTP API

type server struct {
	reg      *registry
	prefetch *prefetcher
	log      *log.Logger
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/api/next", s.handleNext)
	mux.HandleFunc("/api/stream", s.handleStream)
	mux.HandleFunc("/api/raw", s.handleRaw)
	mux.HandleFunc("/api/diag", s.handleDiag)
	mux.Handle("/", staticHandler(s.log))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errBody(msg string) map[string]any { return map[string]any{"error": msg} }

func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": appVersion,
		"served":  s.reg.served(),
	})
}

func (s *server) handleNext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("只支持 GET"))
		return
	}
	id, err := s.prefetch.take()
	if err != nil {
		s.log.Printf("取视频失败: %v", err)
		var ue *upstreamError
		if errors.As(err, &ue) {
			writeJSON(w, http.StatusBadGateway, errBody("没能取到新视频："+ue.msg))
			return
		}
		writeJSON(w, http.StatusBadGateway, errBody("没能取到新视频：连不上接口，请检查 NAS 能否访问外网"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    id,
		"count": s.reg.served(),
	})
}

// handleDiag 现场测一次上游连通性。视频刷不出来时，用户点一下就能看到
// 到底卡在哪一步（DNS / 证书 / 接口状态码 / 域名没放行），不用猜。
func (s *server) handleDiag(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	raw, err := fetchRandomVideo(fetchClient())
	ms := time.Since(start).Milliseconds()
	if err != nil {
		s.log.Printf("诊断失败: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": err.Error(),
			"ms":    ms,
			"hosts": hostList(),
		})
		return
	}
	host := ""
	if u, err := url.Parse(raw); err == nil {
		host = u.Hostname()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"host":  host,
		"ms":    ms,
		"hosts": hostList(),
	})
}

// handleRaw 让浏览器直连源站（省一层中转）。只认服务端签发的 id。
func (s *server) handleRaw(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.lookup(w, r)
	if !ok {
		return
	}
	http.Redirect(w, r, raw, http.StatusFound)
}

// handleStream 服务端代理视频字节流，支持 Range。
// 必要性：inner 窗口里 <video> 直连快手 CDN 不一定通（客户端外网策略/防盗链）；
// 让 NAS 去取再吐给浏览器，链路最短且可控。
func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		return
	}

	upReq, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody("构造上游请求失败"))
		return
	}
	upReq.Header.Set("User-Agent", uaForUpstream)
	upReq.Header.Set("Referer", upstreamRef)
	if v := r.Header.Get("Range"); v != "" {
		upReq.Header.Set("Range", v)
	}
	if v := r.Header.Get("Accept"); v != "" {
		upReq.Header.Set("Accept", v)
	}

	upResp, err := proxyClient().Do(upReq.WithContext(r.Context()))
	if err != nil {
		if r.Context().Err() == nil {
			s.log.Printf("代理上游失败: %v", err)
		}
		writeJSON(w, http.StatusBadGateway, errBody("读取视频源失败"))
		return
	}
	defer upResp.Body.Close()

	if upResp.StatusCode < 200 || upResp.StatusCode >= 300 {
		writeJSON(w, http.StatusBadGateway, errBody(fmt.Sprintf("视频源返回 %d", upResp.StatusCode)))
		return
	}

	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "ETag", "Last-Modified"} {
		if v := upResp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "public, max-age=300")
	// 网关 nginx 默认会缓冲响应体，视频会攒够一段才开始播；这个头让它边收边发
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(upResp.StatusCode)

	if _, err := io.Copy(w, upResp.Body); err != nil && r.Context().Err() == nil {
		// 客户端提前断开（切下一个视频）属正常，只在非取消时记录
		s.log.Printf("转发中断: %v", err)
	}
}

// lookup 校验 id 并取出源站地址。
func (s *server) lookup(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("只支持 GET"))
		return "", false
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少 id"))
		return "", false
	}
	raw, ok := s.reg.get(id)
	if !ok {
		writeJSON(w, http.StatusGone, errBody("这条视频的连接已过期，请点下一个"))
		return "", false
	}
	return raw, true
}

// ---------------------------------------------------------------- 传输层

var (
	transportOnce sync.Once
	transportVal  *http.Transport
)

func sharedTransport() *http.Transport {
	transportOnce.Do(func() {
		transportVal = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			DisableCompression:    true,
		}
	})
	return transportVal
}

// fetchClient 故意【不跟随重定向】：随机接口返回 302 + Location，我们要的正是那个 Location。
// Go 默认会一路跟到 CDN，把整个视频体拉下来后才返回 200。
func fetchClient() *http.Client {
	return &http.Client{
		Timeout:   fetchTimeout,
		Transport: sharedTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// proxyClient 取视频字节流，允许跟随少量重定向，但每一跳都要再过一遍 host 白名单。
func proxyClient() *http.Client {
	return &http.Client{
		Timeout:   proxyTimeout,
		Transport: sharedTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("重定向次数过多")
			}
			if req.URL.Scheme != "https" || !hostAllowed(req.URL.Hostname()) {
				return errors.New("重定向到了未放行的域名")
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------- main

func main() {
	port := flag.Int("port", 21166, "监听端口")
	flag.Parse()

	prepareEnv()

	lg := log.New(os.Stdout, "[randomvideo] ", log.LstdFlags)
	lg.Printf("启动 v%s，端口 %d", appVersion, *port)
	lg.Printf("CDN 放行名单: %s", strings.Join(hostList(), ", "))

	reg := newRegistry()
	pf := newPrefetcher(reg, lg)
	defer pf.close()

	srv := &server{reg: reg, prefetch: pf, log: lg}

	httpSrv := &http.Server{
		Addr:              "0.0.0.0:" + strconv.Itoa(*port),
		Handler:           logRequests(lg, srv.routes()),
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      0, // 视频流不能被写超时掐断
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		if err != nil && strings.Contains(err.Error(), "address already in use") {
			lg.Printf("致命：端口 %d 已被占用。请在应用设置里换端口，或先停掉占用它的程序", *port)
		} else {
			lg.Printf("致命：服务退出 %v", err)
		}
		os.Exit(1)
	case <-time.After(400 * time.Millisecond):
		lg.Printf("已监听 0.0.0.0:%d", *port)
	}

	stop := make(chan os.Signal, 2)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	sig := <-stop
	lg.Printf("收到信号 %v，正在退出", sig)

	pf.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		lg.Printf("关闭服务: %v", err)
	}
	lg.Print("已停止")
}

// prepareEnv 沙箱里没有 /tmp，把临时目录指到平台给的可写目录。
func prepareEnv() {
	base := os.Getenv("UGAPP_CACHE_DIR")
	if base == "" {
		base = os.Getenv("UGAPP_DATA_DIR")
	}
	if base == "" {
		return
	}
	tmp := filepath.Join(base, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err == nil {
		_ = os.Setenv("TMPDIR", tmp)
	}
}

func hostList() []string {
	out := append([]string{}, allowedHostSuffixes...)
	out = append(out, extraHosts()...)
	sort.Strings(out)
	return out
}

func logRequests(lg *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 视频代理流量不打日志，避免刷屏
		if r.URL.Path != "/api/stream" {
			lg.Printf("%s %s", r.Method, r.URL.RequestURI())
		}
		next.ServeHTTP(w, r)
	})
}
