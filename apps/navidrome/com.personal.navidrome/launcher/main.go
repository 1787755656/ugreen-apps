// Navidrome for UGOS Pro — 管理壳 launcher
//
// 职责（全部集中在 Go，start.sh 只做 GODEBUG 消毒后 exec 本程序）：
//  1. 解析音乐库目录参数（type: path）+ 兜底共享目录
//  2. 首启授权自愈：读 unit 的 BindPaths=（引号坑：`-` 在引号前；路径可能含
//     空格，全部子串匹配不分词），声明了但沙箱看不到 → exit 3 借 systemd
//     Restart=on-failure 5s 后拉起；10 分钟窗口最多 2 次防循环
//  3. 把 navidrome 拉在 127.0.0.1:14533（沙箱内回环），env 全部钉死
//  4. 反代 0.0.0.0:24533 → navidrome：
//     - FlushInterval=-1，SSE(/api/events) 与音频流实时透传
//     - SPA fallback：/app/* 下无扩展名的 HTML 导航请求若 404（上游
//     frontendAssetsHandler 没有 fallback，/app/login 等深链会裸 404，
//     浏览器地址栏残留在这种 URL 就永远卡死），改发缓存的 index.html
//  5. /api/diag（仅同机访问）——鉴权/挂载排障入口
//  6. 信号转发：SIGTERM → 子进程，双进程优雅退出
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

const (
	appID        = "com.personal.navidrome"
	externalPort = "24533" // 必须与 project.yaml 的 port 一致
	internalPort = "14533" // navidrome 只绑沙箱内回环
	unitFile     = "/lib/systemd/system/" + appID + ".service"
	healWindow   = 600 // 秒
	healMax      = 2
)

var (
	reQuoted   = regexp.MustCompile(`-*"([^"]*)"`) // BindPaths 引号 token（`-` 在引号前）
	reLeadDash = regexp.MustCompile(`(^|\s)-`)     // token 起始的忽略标记 `-`
	reAppData  = regexp.MustCompile(`@app`)        // 平台自有目录
	reVolume   = regexp.MustCompile(`/volume[0-9]`)
)

func logf(format string, a ...any) {
	fmt.Printf("[launcher] "+format+"\n", a...)
}

// ---- 子进程输出环形缓冲：/api/diag 可回看最近日志（转码错误/panic 都在这里）----
type ringBuf struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newRingBuf(capacity int) *ringBuf { return &ringBuf{cap: capacity} }

func (r *ringBuf) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
	return len(p), nil
}

func (r *ringBuf) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// .env 参数值清洗：没选是字面量 null；兼容 ["..."] 数组形态
func cleanPath(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "null" || v == "Null" || v == "NULL" || v == "[]" {
		return ""
	}
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	}
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		v = strings.Trim(v, `"`)
	}
	if i := strings.Index(v, `",`); i >= 0 { // 多元素数组只取第一项
		v = strings.TrimPrefix(v[:i], `"`)
	}
	return strings.TrimSpace(v)
}

// unitBindPathsRaw：读 unit 里所有 BindPaths= 值，解引号、去 `-` 前缀，空格规整。
// 不分词 —— 用户路径可能含空格，一切判定走子串匹配。
func unitBindPathsRaw() string {
	data, err := os.ReadFile(unitFile)
	if err != nil {
		return ""
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "BindPaths=") {
			continue
		}
		val := strings.TrimPrefix(line, "BindPaths=")
		val = reQuoted.ReplaceAllString(val, " $1 ")
		val = reLeadDash.ReplaceAllString(val, "$1")
		out = append(out, val)
	}
	return strings.Join(out, " ")
}

func unitHasUserRoot(raw string) bool {
	if raw == "" {
		return false
	}
	remain := raw
	for _, tok := range strings.Fields(raw) {
		if reAppData.MatchString(tok) {
			remain = strings.ReplaceAll(remain, tok, " ")
		}
	}
	return reVolume.MatchString(remain)
}

type healState struct{ last, count int64 }

func readHealState(path string) healState {
	b, err := os.ReadFile(path)
	if err != nil {
		return healState{}
	}
	parts := strings.SplitN(strings.TrimSpace(string(b)), ":", 2)
	last, _ := strconv.ParseInt(parts[0], 10, 64)
	cnt, _ := strconv.ParseInt(parts[1], 10, 64)
	return healState{last: last, count: cnt}
}

// resolveMusicDir：参数可见就用参数；否则退回共享目录。
// 返回 (最终目录, 是否需要自愈)
func resolveMusicDir(dataDir, sharedDir string) (string, bool) {
	userDir := cleanPath(os.Getenv("MUSIC_DIR"))
	raw := unitBindPathsRaw()

	declared := false
	visible := false
	if userDir != "" && raw != "" {
		hay := " " + raw + " "
		declared = strings.Contains(hay, " "+userDir+" ")
	}
	if userDir != "" {
		if st, err := os.Stat(userDir); err == nil && st.IsDir() {
			visible = true
		}
	}

	needHeal := (declared && !visible) ||
		(userDir == "" && unitHasUserRoot(raw)) // unit 已声明用户目录但参数还没写入 = 首启指纹
	if needHeal {
		if declared && !visible {
			logf("[自愈] 音乐库目录 %s 已被平台授权但沙箱里还看不到（首启/刚改参数）", userDir)
		} else {
			logf("[自愈] unit 已声明用户授权目录但参数值还没写入（首启指纹）")
		}
	}

	final := ""
	if userDir != "" && visible {
		final = userDir
		logf("音乐库目录：%s（安装时选择）", final)
	} else if userDir != "" {
		logf("警告：选择的音乐库目录 %s 在沙箱里不可见（授权未生效或目录已删除）。", userDir)
	}
	if final == "" {
		if sharedDir != "" {
			final = sharedDir
		} else {
			final = filepath.Join(dataDir, "music")
		}
		if err := os.MkdirAll(final, 0755); err != nil {
			logf("错误：无法创建回退音乐库目录 %s：%v", final, err)
		}
		logf("音乐库目录：%s（回退目录；到应用中心「配置」里选择音乐库目录，重启应用生效）", final)
	}
	_ = os.MkdirAll(final, 0755)
	return final, needHeal
}

func maybeHeal(dataDir string, needHeal bool) {
	if !needHeal {
		_ = os.Remove(filepath.Join(dataDir, ".selfheal_state"))
		return
	}
	mark := filepath.Join(dataDir, ".selfheal_state")
	st := readHealState(mark)
	now := time.Now().Unix()
	if now-st.last > healWindow {
		st.count = 0
	}
	if st.count >= healMax {
		logf("[自愈] %d 分钟内已自愈 %d 次仍未就绪，放弃自愈继续启动。", healWindow/60, st.count)
		logf("        请到应用中心「停止 → 启动」，或检查应用设置里的音乐库目录。")
		return
	}
	logf("[自愈] 主动退出一次，等 systemd 拉起（RestartSec=5s）；本次窗口第 %d 次（上限 %d）", st.count+1, healMax)
	_ = os.WriteFile(mark, []byte(fmt.Sprintf("%d:%d\n", now, st.count+1)), 0644)
	time.Sleep(500 * time.Millisecond)
	os.Exit(3)
}

func sanitizeGodebug() {
	raw := os.Getenv("GODEBUG")
	if raw == "" {
		return
	}
	removed := map[string]bool{
		"x509sha1": true, "tlskyber": true, "gotypesalias": true, "tlsunsafeekm": true,
		"tlsrsakex": true, "tls3des": true, "tls10server": true, "x509keypairleaf": true,
		"asynctimerchan": true,
	}
	var keep []string
	for _, item := range strings.Split(raw, ",") {
		k := strings.SplitN(item, "=", 2)[0]
		if removed[k] {
			continue
		}
		keep = append(keep, item)
	}
	if len(keep) == 0 {
		_ = os.Unsetenv("GODEBUG")
	} else {
		_ = os.Setenv("GODEBUG", strings.Join(keep, ","))
	}
}

func isSameHost(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
			return true
		}
	}
	return false
}

func main() {
	sanitizeGodebug()
	logf("==== Navidrome for UGOS Pro，launcher %s ====", version)

	installDir := envOr("UGAPP_INSTALL_DIR", "/volume1/@appstore/"+appID)
	dataDir := envOr("UGAPP_DATA_DIR", filepath.Join(installDir, "data"))
	logDir := envOr("UGAPP_LOG_DIR", filepath.Join(installDir, "log"))
	cacheDir := envOr("UGAPP_CACHE_DIR", filepath.Join(dataDir, "cache"))
	sharedDir := envOr("UGAPP_SHARED_DIR", "")

	for _, d := range []string{dataDir, logDir, cacheDir} {
		_ = os.MkdirAll(d, 0755)
	}
	tmpDir := filepath.Join(cacheDir, "tmp")
	_ = os.MkdirAll(tmpDir, 0755)

	musicDir, needHeal := resolveMusicDir(dataDir, sharedDir)
	maybeHeal(dataDir, needHeal) // 需要 self-heal 时在这里 exit 3，绝不先拉起 navidrome

	// ---- 钉死 navidrome 环境 ----
	bin := filepath.Join(installDir, "bin", "navidrome")
	ffmpegDir := filepath.Join(installDir, "ffmpeg")
	os.Setenv("ND_PORT", internalPort)
	os.Setenv("ND_ADDRESS", "127.0.0.1") // 只绑回环，全部外部流量走 launcher
	os.Setenv("ND_MUSICFOLDER", musicDir)
	os.Setenv("ND_DATAFOLDER", dataDir)
	os.Setenv("ND_CACHEFOLDER", cacheDir)
	os.Setenv("ND_LOGLEVEL", "info")
	os.Setenv("ND_GATRACKINGID", "") // 关闭 Google Analytics 上报
	// 0.64.2 稳定性叠加（均为上游环境变量，不改二进制）：
	// - 进程内扫描器：外部扫描子进程用 gob 管道传状态，任何异常都会以
	//   "failed to read status from scanner: gob: ..." 形式炸到 UI；进程内扫描
	//   彻底消灭这一类错误。代价是扫描内存留在主进程，NAS 场景可接受。
	os.Setenv("ND_DEVEXTERNALSCANNER", "false")
	// - 关外部服务：艺术家资料/Last.fm/Deezer 在国内 NAS 上基本全部超时
	//   （日志里 deezer 请求反复 deadline），只贡献无意义的并发 DB 压力，
	//   而并发正是 0.64.2 sqlite 语句竞态 panic 的触发条件之一。
	//   需要 Last.fm/ListenBrainz 的用户可自行改启动脚本对应行。
	os.Setenv("ND_ENABLEEXTERNALSERVICES", "false")
	os.Setenv("ND_ENABLEINSIGHTSCOLLECTOR", "false")
	os.Setenv("TMPDIR", tmpDir)
	if _, err := os.Stat(filepath.Join(ffmpegDir, "ffmpeg")); err == nil {
		os.Setenv("ND_FFMPEGPATH", filepath.Join(ffmpegDir, "ffmpeg"))
		os.Setenv("PATH", ffmpegDir+":"+os.Getenv("PATH"))
	} else {
		logf("警告：未找到捆绑的 ffmpeg（%s），实时转码不可用。", ffmpegDir)
	}
	if ca := filepath.Join(installDir, "ca", "ca-certificates.crt"); fileExists(ca) {
		os.Setenv("SSL_CERT_FILE", ca)
	} else {
		logf("警告：包内 CA 证书缺失，出站 HTTPS（艺术家资料/Last.fm 等）将失败。")
	}

	// ---- 拉起 navidrome ----
	appLog := newRingBuf(256 * 1024)
	childReady := make(chan struct{})
	var child *exec.Cmd
	var childErr error
	go func() {
		child = exec.Command(bin)
		child.Stdout = io.MultiWriter(os.Stdout, appLog)
		child.Stderr = io.MultiWriter(os.Stderr, appLog)
		if err := child.Start(); err != nil {
			childErr = err
			close(childReady)
			return
		}
		close(childReady)
		logf("navidrome 已拉起（127.0.0.1:%s，数据 %s）", internalPort, dataDir)
		if err := child.Wait(); err != nil {
			logf("navidrome 进程退出：%v —— launcher 随之退出，交由 systemd 拉起整个应用", err)
		} else {
			logf("navidrome 正常退出")
		}
		os.Exit(3) // 子进程退出 = 应用终结，非零码让 Restart=on-failure 把整套拉起
	}()
	<-childReady
	if childErr != nil {
		logf("错误：无法启动 %s：%v", bin, childErr)
		os.Exit(1)
	}

	// ---- 反代 + SPA fallback ----
	target, _ := url.Parse("http://127.0.0.1:" + internalPort)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // SSE / 音频流逐块即时冲刷
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logf("后端暂不可达：%s %s：%v", r.Method, r.URL.Path, err)
		w.WriteHeader(http.StatusBadGateway)
	}

	var indexMu sync.RWMutex
	var indexHTML []byte
	fetchIndex := func() []byte {
		indexMu.Lock()
		defer indexMu.Unlock()
		resp, err := http.Get(target.String() + "/app/")
		if err != nil {
			return indexHTML
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			if b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)); err == nil && len(b) > 0 {
				indexHTML = b
			}
		}
		return indexHTML
	}
	go func() { // 后台预热，失败不阻塞启动
		for i := 0; i < 120; i++ {
			if fetchIndex() != nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	reNoExt := regexp.MustCompile(`[^./]+\.[^./]+$`) // 最后一段含扩展名
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode != http.StatusNotFound || resp.Request == nil || resp.Request.Method != http.MethodGet {
			return nil
		}
		p := resp.Request.URL.Path
		if !strings.HasPrefix(p, "/app/") || p == "/app/" {
			return nil
		}
		last := p[strings.LastIndex(p, "/")+1:]
		if last == "" || reNoExt.MatchString(last) {
			return nil // 真资源 404 原样透传
		}
		idx := fetchIndex()
		if len(idx) == 0 {
			return nil
		}
		logf("[SPA fallback] %s 裸 404 → 改发 index.html（上游 frontendAssetsHandler 无 fallback）", p)
		// 旧 404 body 可能是 gzip 的（compress 中间件按请求 Accept-Encoding 决定），
		// 换 body 必须同步删 Content-Encoding，否则浏览器按 gzip 解纯文本 →
		// ERR_CONTENT_DECODING_FAILED，页面永远白屏。旧 body 排空后关闭防连接泄漏。
		resp.Header.Del("Content-Encoding")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		resp.StatusCode = http.StatusOK
		resp.Status = "200 OK"
		resp.Body = io.NopCloser(bytes.NewReader(idx))
		resp.ContentLength = int64(len(idx))
		resp.Header.Set("Content-Length", strconv.Itoa(len(idx)))
		resp.Header.Set("Content-Type", "text/html; charset=utf-8")
		return nil
	}

	mux := http.NewServeMux()
	// getTranscodeDecision 旁路：0.64.2 的决策接口在并发下有 sqlite 语句复用 panic
	// （makeslice: len out of range，进程直接崩），且实测对浏览器原生格式也会给出
	// "no compatible playback profile found"。这里回一个合法 ok 决策（无 transcodeParams），
	// UI 自动回退到 /rest/stream（经典播放路径，raw/转码/Range 已验证全通）。
	// 上游修复决策接口后可移除此旁路。
	mux.HandleFunc("/rest/getTranscodeDecision", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"subsonic-response":{"status":"ok","version":"1.16.1","type":"navidrome","serverVersion":"0.64.2 (10114574)","openSubsonic":true}}`)
	})
	mux.HandleFunc("/api/diag", func(w http.ResponseWriter, r *http.Request) {
		// 局域网可读：diag 只含版本/存活/目录/最近日志，无凭据类信息；
		// 该应用无 SSH 排障通道，浏览器直开是唯一排查入口。
		alive := child != nil && child.Process != nil && child.ProcessState == nil
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, `{"app":%q,"launcher":%q,"navidromeAlive":%t,"internal":"127.0.0.1:%s","musicDir":%q,"decisionBypass":true,"inProcessScanner":true,"externalServices":false,"recentLog":%q}`+"\n",
			appID, version, alive, internalPort, musicDir, appLog.String())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{Addr: "0.0.0.0:" + externalPort, Handler: mux} // 不设读写超时：音频/SSE 长流
	go func() {
		logf("监听 0.0.0.0:%s → 127.0.0.1:%s", externalPort, internalPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logf("错误：监听失败：%v", err)
			os.Exit(1)
		}
	}()

	// ---- 信号：转发给子进程，双进程优雅退出 ----
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	logf("收到 %v，转发给 navidrome 并退出", sig)
	if child != nil && child.Process != nil {
		_ = child.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = child.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = child.Process.Kill()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	logf("已退出")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
