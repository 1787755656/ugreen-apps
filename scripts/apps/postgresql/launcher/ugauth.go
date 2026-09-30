package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 接入 UGOS Pro 的登录认证能力，不再自己实现账号系统。
//
// 官方机制（doc: backend/system-capabilities/login-auth）：
//
//	前端 cloudWindow.useCapacity('getThirdToken') 拿到 third_token
//	  → 以 Ugreen-Ttk 请求头带上，请求 /api/**（即 project.yaml 的 proxy_path）
//	  → 系统网关校验登录态，通过后把用户信息注入请求头再转发给我们：
//	        Ugreen-User-ID    用户 ID
//	        Ugreen-User-Name  用户名
//	        Ugreen-User-Type  admin / users
//
// 【这套东西可信的前提是：网关是唯一入口。】
//
// 2026-08-06 真机实测过：open_type: inner 的应用，它声明的那个端口在局域网上
// 【直接就能连上】（course-manager 的 21140 从另一台机器 curl 直接 200）。
// 也就是说网关只是其中一条路。谁都可以绕过网关直连端口、自己伪造这三个头。
//
// 所以照标准做法设【两道独立的闸】，两道都过才放行：
//
//	第一道 只绑 127.0.0.1 + requireSameHost：请求必须来自 NAS 本机。
//	第二道 requireAuthed：                   请求必须带着网关注入的用户身份。
//
// 绑回环这一条在本应用上【已经真机验证过】（绑上之后请求照常到达后端，
// 说明网关的 proxy_pass 确实走回环）。requireSameHost 在绑了回环之后理论上
// 永远为真，留着它不是冗余而是【保险丝】：哪天有人为了调试把监听地址改回
// 0.0.0.0，鉴权不会跟着一起塌掉。这类"改一行看似无关的代码就打开一个洞"的事故
// 在 MariaDB 那个应用上真实发生过 —— /api/status 把数据库密码明文暴露给了
// 同网段任何设备。
//
// 残留的暴露面：同一台 NAS 上的【其它应用】仍然能访问 127.0.0.1:<port> 并伪造头。
// 挡的是局域网上的其它机器，不是同机的其它应用 —— 那些是管理员自己装的。

type ugUser struct {
	ID   string
	Name string
	Type string
}

// ugUserFrom 从网关注入的请求头里取用户身份。
// 头名大小写无所谓：Go 在解析和 Get 时都会做同样的规范化。
func ugUserFrom(r *http.Request) (ugUser, bool) {
	u := ugUser{
		ID:   r.Header.Get("Ugreen-User-ID"),
		Name: r.Header.Get("Ugreen-User-Name"),
		Type: r.Header.Get("Ugreen-User-Type"),
	}
	return u, u.ID != ""
}

// localAddrCache 缓存本机所有网卡地址，用来判断请求是不是同机发来的。
//
// 缓存一小段时间：网卡地址会变（DHCP 续租、插拔网线），但也没必要每个请求
// 都去问一次内核。
type localAddrCache struct {
	mu       sync.Mutex
	addrs    map[string]bool
	loadedAt time.Time
}

var localAddrs localAddrCache

const localAddrTTL = 60 * time.Second

func (c *localAddrCache) contains(ip net.IP) bool {
	// 回环永远算本机，不依赖网卡枚举 —— 枚举失败时这条兜底最要紧，
	// 因为我们只绑回环，网关必然是从回环连过来的。
	if ip.IsLoopback() {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.addrs == nil || time.Since(c.loadedAt) > localAddrTTL {
		if addrs, err := net.InterfaceAddrs(); err == nil {
			fresh := map[string]bool{}
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok {
					fresh[ipnet.IP.String()] = true
				}
			}
			c.addrs = fresh
			c.loadedAt = time.Now()
		} else if c.addrs == nil {
			// 一次都没读到过就当"只认回环"：宁可拒错，也不放错。
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

// blockedPageHTML 是给"不是从 NAS 本机来的"请求看的整页。
//
// 刻意做成一张【什么都不透露】的页面：不出现应用名、版本、端口、任何配置项，
// 也不暗示管理页面长什么样。局域网上的人扫到这个端口，只应该知道"此处不对外"。
// 页面骨架本身就是信息 —— 表单字段、连接串模板、各种文案会把"这台 NAS 上跑着
// 什么、能配什么"全交代出来。
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

// sameHost 是第一道闸：只接受来自 NAS 本机的请求。
func (a *App) sameHost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isSameHostRequest(r) {
			// 话术不提"你被当成攻击者了"：走到这里的绝大多数其实是好奇心驱使、
			// 直接敲了端口号的用户本人。
			writeErr(w, http.StatusForbidden,
				"本接口只接受来自 NAS 本机的请求。请从 NAS 桌面的应用图标打开本应用，"+
					"不要直接访问端口号。")
			return
		}
		next(w, r)
	}
}

// auth 包住所有需要登录的接口 —— 两道闸都要过。
//
// 【只判断"网关认证过没有"，不再自己判断是不是管理员。】
//
// 原来这里额外要求 Ugreen-User-Type == "admin"（文档写的是 admin=管理员、
// users=普通用户）。真机上把一个【管理员用户组】的账号直接挡在了外面 ——
// 说明这个字段区分的不是"有没有管理权限"，"管理员账号"和"管理员组成员"
// 在它上面不是一回事。拿一个没在真机上验证过的字符串做硬拦截，
// 结果就是合法用户进不来，而这种失败还特别难自查。
//
// 现在谁能进由平台决定：project.yaml 里 only_admin: true，由系统去管
// 这个应用对谁可见、谁能打开。我们只确认"请求确实经过了网关的鉴权"。
//
// 注意【探活接口不能包在这里面】：系统探测应用是否存活时是直接打端口的，
// 不经过网关，也就带不上那几个头。包进来会让应用一直被判定为"未启动"。
func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return a.sameHost(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ugUserFrom(r); !ok {
			writeErr(w, http.StatusUnauthorized,
				"没有拿到绿联登录信息，请从绿联云桌面里点击应用图标打开。"+
					"如果本来就是这么打开的，访问 /api/diag 可以看到网关到底传了什么过来。")
			return
		}
		next(w, r)
	})
}

// postOnly 把只该用 POST 的接口钉死。
//
// 【为什么不能省】：这些 handler 里有一半根本不读请求体（比如「立即备份」），
// 一个 GET 就能触发一次几分钟的全量导出。更要紧的是 GET 可以被 <img src>、
// 页面跳转这类东西无声地发出去，而 POST + 自定义认证头会强制走 CORS 预检 ——
// 换句话说，这一行同时是 CSRF 防线。
func postOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeErr(w, http.StatusMethodNotAllowed, "这个接口只接受 POST")
			return
		}
		next(w, r)
	}
}

// getOnly 同理，用在纯查询的接口上。
func getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeErr(w, http.StatusMethodNotAllowed, "这个接口只接受 GET")
			return
		}
		next(w, r)
	}
}

// handleDiag 回显网关到底注入了哪些 Ugreen-* 请求头。【故意不加鉴权】。
//
// 用户手上没有 SSH，鉴权链路一旦出问题就完全是个黑盒 —— 到底是网关没转发、
// 没注入头，还是某个字段的值和文档不一样，从界面上根本看不出来。
// 这个接口只把【调用方自己发过来的】头回显回去，不泄露任何东西，
// 却能让这一类问题一眼看穿。上面那个"管理员组被挡在外面"的问题
// 就是因为当初没有这个接口，只能靠猜。
func (a *App) handleDiag(w http.ResponseWriter, r *http.Request) {
	headers := map[string]string{}
	for k, v := range r.Header {
		if len(k) >= 7 && strings.EqualFold(k[:7], "ugreen-") && len(v) > 0 {
			// Ugreen-Ttk 是凭据，只报长度不报值。
			if strings.EqualFold(k, "Ugreen-Ttk") {
				headers[k] = fmt.Sprintf("（长度 %d 的令牌，不回显）", len(v[0]))
				continue
			}
			headers[k] = v[0]
		}
	}
	u, authed := ugUserFrom(r)
	writeJSON(w, 200, map[string]any{
		"gateway_headers": headers,
		"authenticated":   authed,
		"same_host_ok":    isSameHostRequest(r),
		"user":            map[string]string{"id": u.ID, "name": u.Name, "type": u.Type},
		"remote_addr":     r.RemoteAddr,
		"hint": "same_host_ok 和 authenticated 都为 true 才能访问其它接口。" +
			"gateway_headers 为空 = 请求没经过网关（或网关没鉴权）；" +
			"有 Ugreen-User-ID 就说明鉴权链路是通的。",
	})
}
