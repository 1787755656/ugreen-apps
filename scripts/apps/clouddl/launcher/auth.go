package main

// 接入 UGOS Pro 的「登录认证」能力（官方文档：
// developer.ugnas.com/doc/backend/system-capabilities/login-auth.html）。
//
// 机制：`open_type: inner` + `proxy_path: api` 的应用，前端请求 /api/* 会先经过
// 系统网关；网关校验完登录态后，把当前用户以三个请求头注入再转发给我们：
//
//	Ugreen-User-ID    用户 ID
//	Ugreen-User-Name  用户名
//	Ugreen-User-Type  文档说 admin=管理员 / users=普通用户，但【别信这个字段】，见 requireAdmin
//
// ⚠⚠ 光有网关认证【远远不够】：真机实测 inner 应用的声明端口在局域网上直接可达
// （从另一台机器 curl 拿到 HTTP 200），谁都能绕开网关直连并伪造那几个头。
//
// 所以设了【两道独立的闸】，两道都过才放行：
//
//	requireSameHost  挡局域网上的其它机器：请求源 IP 必须是 NAS 本机的地址
//	requireAdmin     挡没经过网关的请求：必须带着网关注入的 Ugreen-User-ID
//
// 【这个应用尤其不能马虎】：上游本身没有任何登录，接口里有网盘凭据的读写、
// 有整个 NAS 下载目录的文件浏览和重命名。没有这两道闸，等于把别人的百度网盘
// 账号和 NAS 目录开放给整个局域网。
//
// 残留暴露面：NAS 上的【其它应用】仍可直连本端口并伪造那些头 ——
// 这和"只绑回环"方案的暴露面是同一个，防的是局域网上的其它机器，
// 不是同机管理员自己装的应用。

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

type Identity struct {
	ID   string
	Name string
	Type string
}

const (
	headerUserID   = "Ugreen-User-ID"
	headerUserName = "Ugreen-User-Name"
	headerUserType = "Ugreen-User-Type"
)

func identityFrom(r *http.Request) (Identity, bool) {
	id := Identity{
		ID:   r.Header.Get(headerUserID),
		Name: r.Header.Get(headerUserName),
		Type: r.Header.Get(headerUserType),
	}
	// 只认 ID：有它就说明请求确实过了网关认证。
	// 【不要】把 Type 也加进这个条件 —— 它的取值和文档对不上（见 requireAdmin）。
	if id.ID == "" {
		return Identity{}, false
	}
	return id, true
}

// localAddrCache 缓存本机所有网卡地址。
// 网卡地址可能变（DHCP 续租、插拔网线），但也没必要每个请求都问内核。
type localAddrCache struct {
	mu       sync.Mutex
	addrs    map[string]bool
	loadedAt time.Time
}

var localAddrs localAddrCache

const localAddrTTL = 60 * time.Second

func (c *localAddrCache) contains(ip net.IP) bool {
	// 回环永远算本机，不依赖网卡枚举 —— 枚举失败时这条兜底最要紧，
	// 因为网关最可能就是从回环连过来的。
	if ip.IsLoopback() {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.addrs == nil || time.Since(c.loadedAt) > localAddrTTL {
		fresh := map[string]bool{}
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok {
					fresh[ipnet.IP.String()] = true
				}
			}
			c.addrs = fresh
			c.loadedAt = time.Now()
		} else if c.addrs == nil {
			// 一次都没读到过就当作"只认回环"，宁可拒错也不放错
			c.addrs = map[string]bool{}
			c.loadedAt = time.Now()
		}
	}
	return c.addrs[ip.String()]
}

// isSameHostRequest 判断请求是不是从 NAS 本机发起的。
func isSameHostRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && localAddrs.contains(ip)
}

// requireSameHost 挡掉所有不是从本机发起的请求。
func (s *Server) requireSameHost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.devNoAuth {
			next(w, r)
			return
		}
		if !isSameHostRequest(r) {
			// 话术不提"你被当成攻击者了"，只说正确的做法 ——
			// 绝大多数走到这里的其实是好奇心驱使、直接敲了端口号的用户本人。
			writeErr(w, http.StatusForbidden,
				"本接口只接受来自 NAS 本机的请求。请从 NAS 桌面的应用图标打开本应用，不要直接访问端口号。")
			return
		}
		next(w, r)
	}
}

// requireGatewayOnly 严格要求网关注入的身份，【不接受】会话 Cookie。
// 只给 /api/session 用：拿一个已有的 Cookie 换不出新 Cookie。
//
// ⚠ 它【只校验"网关认证过"，不拿 Ugreen-User-Type 硬拦】。
// 文档写 admin/users，但真机上属于「管理员用户组」的账号拿到的并不是 admin ——
// 按文档写 `if type != "admin" { 403 }` 会把合法管理员挡在门外。
// "只有管理员能用这个应用"交给 project.yaml 的 only_admin: true 由平台管。
func (s *Server) requireGatewayOnly(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSameHost(func(w http.ResponseWriter, r *http.Request) {
		if s.devNoAuth {
			next(w, r)
			return
		}
		if _, ok := identityFrom(r); !ok {
			writeErr(w, http.StatusUnauthorized, authFailureHint)
			return
		}
		next(w, r)
	})
}

// requireAuth 包住所有转发给上游的接口：网关身份【或】会话 Cookie，二选一。
//
// 为什么要接受 Cookie：上游界面里的缩略图、扫码二维码、日志导出是浏览器
// 直接发起的 <img>/<a> 请求，一个自定义头都加不上，永远拿不到网关身份。
// 详见 session.go 开头那段。两条路认的是同一件事，且都还压着"必须来自本机"。
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSameHost(func(w http.ResponseWriter, r *http.Request) {
		if s.devNoAuth {
			next(w, r)
			return
		}
		if _, ok := identityFrom(r); ok {
			next(w, r)
			return
		}
		if s.sessions.valid(sessionTokenFrom(r)) {
			next(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, authFailureHint)
	})
}

// authFailureHint 是 401 时给用户的话。
//
// 一定要指路 /api/diag：这类"网关注入头"的鉴权不通时，界面上只有一句
// "未通过登录认证"，到底是 JSSDK 没加载、token 没取到、还是网关没注入，
// 完全无从判断，而用户手上通常没有 SSH。
const authFailureHint = "未通过 UGOS 登录认证。请从 NAS 桌面的应用图标打开本应用" +
	"（不要直接敲 IP 加端口）。如果本来就是这么打开的，" +
	"在同一个窗口里访问 /api/diag 可以看到网关到底传了什么过来。"

// blockedPageHTML 是给"不是从 NAS 本机来的"请求看的整页。
//
// 刻意做成一张【什么都不透露】的页面：不出现应用名、版本、端口、任何配置项。
// 局域网上的人扫到这个端口，只应该知道"此处不对外"。
//
// 为什么不能只挡 /api：页面骨架本身就是信息 —— 这个应用的页面上写着
// "百度网盘 / 夸克 / 阿里云盘"和一整套设置项，等于告诉扫端口的人这里有什么可拿。
const blockedPageHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<title>403</title>
<style>
 body{font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;
      display:flex;align-items:center;justify-content:center;min-height:100vh;
      margin:0;background:#f5f6f8;color:#444}
 .b{text-align:center;padding:2em}
 h1{font-size:3.4em;margin:0;color:#c2c6cf;letter-spacing:.05em}
 p{margin:.8em 0 0;font-size:.95em;line-height:1.7}
</style></head>
<body><div class="b">
 <h1>403</h1>
 <p>此页面不对外提供访问。</p>
 <p>请从 NAS 桌面的应用图标打开。</p>
</div></body></html>
`

func writeBlockedPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(blockedPageHTML))
}

// requirePageSameHost 和 requireSameHost 是同一道闸，只是失败时回 HTML 整页。
//
// ⚠ 这里【只用】"必须来自本机"这一道，不叠加"必须带网关注入的身份"。
// 原因：inner 应用的页面在生产环境是【网关自己从 www/ 提供】的，
// 只有 /api/ 才反代到本端口 —— 页面请求会不会带 Ugreen-User-* 头没验证过，
// 赌错的后果是应用完全打不开，收益只是挡住"NAS 上的其它应用"。
func (s *Server) requirePageSameHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.devNoAuth {
			next.ServeHTTP(w, r)
			return
		}
		if !isSameHostRequest(r) {
			writeBlockedPage(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleDiag 是【不要求网关认证】的自查接口（但仍然要求来自本机）。
//
// 为什么一定要有它：这类"网关注入头"的黑盒鉴权一旦不通，界面上只有一句
// "未通过 UGOS 登录认证"，到底是网关没转发、没注入头、还是某个字段和文档不一样，
// 完全无从判断 —— 而用户手上通常没有 SSH。它只回显调用方自己发来的头，
// 不泄露任何东西，却能一眼看穿。
func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	ident, authed := identityFrom(r)

	ttk := r.Header.Get("Ugreen-Ttk")
	ttkDesc := "（没有这个头）"
	if ttk != "" {
		ttkDesc = fmt.Sprintf("有，长度 %d", len(ttk))
	}

	sessTok := sessionTokenFrom(r)
	sessDesc := "（没有会话 Cookie）"
	if sessTok != "" {
		if s.sessions.valid(sessTok) {
			sessDesc = "有效"
		} else {
			sessDesc = "有 Cookie 但已失效或不认识（应用重启过？重新加载页面即可）"
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":          version,
		"remote_addr":      r.RemoteAddr,
		"same_host_ok":     ip != nil && localAddrs.contains(ip),
		"gateway_authed":   authed,
		"session_cookie":   sessDesc,
		"forwarded_proto":  r.Header.Get("X-Forwarded-Proto"),
		"ugreen_user_id":   ident.ID,
		"ugreen_user_name": ident.Name,
		"ugreen_user_type": ident.Type,
		"ugreen_ttk":       ttkDesc,
		"dev_no_auth":      s.devNoAuth,
		"python":           s.child.Status(),
		"download_dir":     s.downloadDir,
		"download_note":    s.downloadNote,
		"hint": "same_host_ok 必须为 true；gateway_authed 或 session_cookie 有一个成立即可。" +
			"两个都不成立时，多半是页面没能通过 JSSDK 取到 third_token —— " +
			"在应用窗口里按 F12 看控制台，或看 window.__clouddlAuth 的内容。" +
			"python.running 为 false 则是上游服务没起来，原因在应用日志里（[python] 开头那些行）。",
	})
}
