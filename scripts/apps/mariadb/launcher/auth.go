package main

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// 接入 UGOS Pro 的「登录认证」能力（官方文档：
// developer.ugnas.com/doc/backend/system-capabilities/login-auth.html）。
//
// 机制：`open_type: inner` + `proxy_path: api` 的应用，前端请求 /api/* 时会先经过
// 系统网关；网关校验完登录态后，把当前用户信息以三个请求头注入再转发给我们：
//
//	Ugreen-User-ID    用户 ID
//	Ugreen-User-Name  用户名
//	Ugreen-User-Type  文档说 admin=管理员 / users=普通用户，但【别信这个字段】，
//	                  见下面 requireAdmin 的注释
//
// 前端那侧用 JSSDK 的 getThirdToken 取 token，以 Ugreen-Ttk 头带上（见 web/index.html）。
//
// ⚠⚠ 这套机制成立的【前提】是我们的端口不能被绕过。
//
// 2026-08-06 真机实测：`inner` 应用的声明端口在局域网上是【直接可达】的 ——
// 从另一台机器 curl `http://NAS:21140/`（course-manager，inner）拿到 HTTP 200。
// 也就是说网关只是"其中一条路"，不是唯一入口。谁都可以直连端口并自己伪造
// `Ugreen-User-Type: admin` 这个头。
//
// 所以这里设了【两道独立的闸】，两道都过才放行：
//
//	第一道 requireSameHost：请求的源地址必须是 NAS 本机的地址（回环或本机任一网卡）。
//	                        网关跑在同一台机器上，不管它从哪个地址连过来，源 IP
//	                        都是本机的；而局域网上的攻击者必然不是。
//	第二道 requireAdmin：    请求必须带着网关注入的用户身份（即确实经过了网关认证）。
//
// 为什么是"源地址检查"而不是"只绑 127.0.0.1"：
// 后者更简单也更强，但它成立的前提是【网关的 proxy_pass 走回环】。
// 源地址检查对"网关走回环"和"网关走本机 LAN IP"两种情况都成立，不需要赌。
// 两者不冲突，确认过之后把 --listen 收到 127.0.0.1 可以再叠一层。
//
// 残留暴露面：NAS 上的【其它应用】仍然可以直连本端口并伪造那些头。
// 这和"只绑回环"方案的暴露面是同一个 —— 防的是局域网上的其它机器，
// 不是同一台 NAS 上管理员自己装的应用。
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
	// 【不要】把 Type 也加进这个条件 —— 它的取值和文档对不上（见 requireAdmin），
	// 万一某类用户根本不带这个头，就会把人无谓地挡在门外。
	if id.ID == "" {
		return Identity{}, false
	}
	return id, true
}

// localAddrs 缓存本机所有网卡地址，用于判断请求是不是同机发来的。
// 缓存一小段时间：网卡地址可能变（DHCP 续租、插拔网线），但也没必要每个请求都问内核。
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

// requireSameHost 挡掉所有不是从本机发起的请求。见本文件顶部那段说明。
func (a *Admin) requireSameHost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.devNoAuth {
			next(w, r)
			return
		}
		if !isSameHostRequest(r) {
			// 话术不提"你被当成攻击者了"，只说正确的做法 ——
			// 绝大多数走到这里的其实是好奇心驱使、直接敲了端口号的用户本人。
			writeErr(w, http.StatusForbidden,
				"本接口只接受来自 NAS 本机的请求。请从 NAS 桌面的应用图标打开本应用，"+
					"不要直接访问端口号。")
			return
		}
		next(w, r)
	}
}

// requireAdmin 包住所有会返回敏感信息或改变状态的接口。
//
// ⚠ 它【只校验"网关认证过"，不拿 Ugreen-User-Type 硬拦】。
//
// 文档写的是 admin=管理员 / users=普通用户，但真机上一个属于「管理员用户组」
// 的账号拿到的并不是 admin —— 按文档写 `if type != "admin" { 403 }`
// 会把合法的管理员直接挡在门外。这个字段区分的显然不是"有没有管理权限"，
// "管理员账号"和"管理员组成员"是两回事。
//
// 所以"只有管理员能用这个应用"这件事交给 project.yaml 的 only_admin: true
// 由平台去管（谁可见、谁能打开）；这里只负责确认请求确实经过了网关认证。
// 配合上面那道"必须来自本机"的闸，未授权的人根本到不了这里。
//
// 真实取值可以用 /api/diag 在真机上看（那个接口不鉴权、也不吐任何敏感信息）。
// 等确认了管理员组成员实际拿到什么值，再考虑要不要收紧。
func (a *Admin) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return a.requireSameHost(func(w http.ResponseWriter, r *http.Request) {
		if a.devNoAuth {
			next(w, r)
			return
		}
		if _, ok := identityFrom(r); !ok {
			// 走到这里通常不是"攻击"，而是用户直接访问了端口号，
			// 或者前端没能取到认证 token。话术要指向排查方向。
			writeErr(w, http.StatusUnauthorized,
				"未通过 UGOS 登录认证。请从 NAS 桌面的应用图标打开本应用。"+
					"如果本来就是这么打开的，访问 /api/diag 可以看到网关到底传了什么过来。")
			return
		}
		next(w, r)
	})
}

// blockedPageHTML 是给"不是从 NAS 本机来的"请求看的整页。
//
// 刻意做成一张【什么都不透露】的页面：不出现应用名、版本、端口、任何配置项，
// 也不暗示管理页面长什么样。局域网上的人扫到这个端口，只应该知道"此处不对外"。
//
// 为什么不能只挡 /api：页面骨架本身就是信息 —— 表单字段、连接串模板、
// 各种文案会把"这台 NAS 上跑着什么、能配什么"全交代出来。
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

// writeBlockedPage 回一张 HTML 错误页（而不是 JSON）。
// 走到这里的多半是浏览器直接敲了端口号，给它一张能看的页面比一坨 JSON 好。
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
// 只有 proxy_path 匹配的 /api/ 才会反代到本端口 —— 也就是说页面请求
// 到底会不会带上 Ugreen-User-* 头，取决于网关对非 /api 路径的处理方式，
// 那个没有真机验证过。赌错的后果是应用完全打不开，而收益只是挡住
// "NAS 上的其它应用"（那和 /api 的暴露面本来就是同一个）。
func (a *Admin) requirePageSameHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.devNoAuth {
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

// isSameHostRequest 判断请求是不是从 NAS 本机发起的。
func isSameHostRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && localAddrs.contains(ip)
}

// handleDiag 是【不鉴权】的自查接口。
//
// 为什么一定要有它：这类"网关注入头"的黑盒鉴权一旦不通，界面上只有一句
// "未通过 UGOS 登录认证"，到底是网关没转发、没注入头、还是某个字段值和
// 文档不一样，完全无从判断 —— 而用户手上通常没有 SSH。这个接口把
// 【调用方自己发来的】头原样回显，不泄露任何东西，却能一眼看穿。
//
// 安全性：它只回显请求方自己发的内容 + 服务端的非敏感状态，
// 不含密码、不含配置。token 只报长度不报值。
func (a *Admin) handleDiag(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"remote_addr":      r.RemoteAddr,
		"same_host_ok":     ip != nil && localAddrs.contains(ip),
		"gateway_authed":   authed,
		"ugreen_user_id":   ident.ID,
		"ugreen_user_name": ident.Name,
		"ugreen_user_type": ident.Type,
		"ugreen_ttk":       ttkDesc,
		"dev_no_auth":      a.devNoAuth,
		"hint": "same_host_ok 和 gateway_authed 都为 true 才能访问其它接口。" +
			"gateway_authed 为 false 说明网关没有注入 Ugreen-User-* 头 —— " +
			"多半是前端没拿到 token（页面控制台里能看到原因）。",
	})
}
