package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAdmin(t *testing.T, devNoAuth bool) (*Admin, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	p := &Paths{Install: dir, Data: dir, Log: dir, Cache: dir, Base: dir}
	cfg := &Config{path: dir + "/launcher.json",
		data: ConfigData{Port: 3306, RootPassword: "SUPER-SECRET-PASSWORD"}}
	srv := NewServer(p, cfg, dir+"/dbdata")
	a := NewAdmin(p, cfg, srv, devNoAuth)
	mux := http.NewServeMux()
	a.Register(mux)
	return a, mux
}

// 网关跑在同一台机器上，请求源地址是本机的；
// httptest 默认给的 192.0.2.1 会被第一道闸挡掉，所以要显式改成回环。
func localReq(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.RemoteAddr = "127.0.0.1:54321"
	return r
}

func adminReq(method, path, body string) *http.Request {
	r := localReq(method, path, body)
	r.Header.Set(headerUserID, "1")
	r.Header.Set(headerUserName, "admin")
	r.Header.Set(headerUserType, "admin")
	return r
}

// 第一道闸：局域网上的机器直连端口，即便把 Ugreen-User-* 头全伪造对了也进不来。
// 这条是整个鉴权设计的地基 —— 因为实测 inner 应用的端口在局域网上直接可达。
func TestRemoteHostRejectedEvenWithForgedHeaders(t *testing.T) {
	_, mux := newTestAdmin(t, false)
	for _, path := range []string{"/api/status", "/api/logs"} {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "192.168.2.77:40000" // 同网段的另一台机器
		req.Header.Set(headerUserID, "1")
		req.Header.Set(headerUserName, "admin")
		req.Header.Set(headerUserType, "admin") // 伪造成管理员
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s 来自局域网的伪造请求得到 %d，期望 403", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "SUPER-SECRET-PASSWORD") {
			t.Errorf("%s 向局域网请求泄露了 root 密码", path)
		}
	}
}

func TestLocalAddrCacheAcceptsLoopbackAndOwnAddrs(t *testing.T) {
	var c localAddrCache
	for _, s := range []string{"127.0.0.1", "::1"} {
		if !c.contains(net.ParseIP(s)) {
			t.Errorf("%s 应该被当成本机地址", s)
		}
	}
	// 一个几乎不可能出现在本机网卡上的地址（文档专用网段）
	if c.contains(net.ParseIP("192.0.2.123")) {
		t.Error("192.0.2.123 不该被当成本机地址")
	}
}

// 这一组是【安全用例】：任何一条挂掉都意味着数据库 root 密码可以被未授权读取。
func TestAPIRequiresUGOSAuth(t *testing.T) {
	_, mux := newTestAdmin(t, false)

	protected := []struct{ method, path string }{
		{"GET", "/api/status"},
		{"GET", "/api/logs"},
		{"POST", "/api/password"},
		{"POST", "/api/network"},
		{"POST", "/api/service"},
	}

	for _, ep := range protected {
		t.Run("无身份_"+ep.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, localReq(ep.method, ep.path, "{}"))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("状态码 = %d，期望 401", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "SUPER-SECRET-PASSWORD") {
				t.Error("未鉴权的响应里泄露了 root 密码")
			}
		})

		// 注意这里【故意】不再断言"Ugreen-User-Type != admin 就 403"。
		// 真机上属于「管理员用户组」的账号拿到的并不是 admin，按文档那么写
		// 会把合法管理员挡在门外。"谁能用这个应用"交给 project.yaml 的
		// only_admin 由平台管；后端只认"有没有经过网关认证"。
		t.Run("网关认证过就放行_"+ep.path, func(t *testing.T) {
			req := localReq(ep.method, ep.path, "{}")
			req.Header.Set(headerUserID, "1001")
			req.Header.Set(headerUserName, "alice")
			req.Header.Set(headerUserType, "users")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("状态码 = %d，不该因为 Ugreen-User-Type 的取值被拦", rec.Code)
			}
		})
	}
}

// 探活接口【不能】要求鉴权：平台的探测请求不经过网关，带不上 Ugreen-User-* 头。
// 要是把它也一起挡了，应用会一直显示"启动失败"。
func TestHealthzIsOpenButLeaksNothing(t *testing.T) {
	_, mux := newTestAdmin(t, false)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, localReq("GET", "/api/healthz", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "ok" {
		t.Errorf("探活接口返回了 %q，它应该只回 ok、不含任何信息", body)
	}
}

// /api/diag 不要求【网关认证】（鉴权链路不通时它是唯一的排查手段），
// 但仍然要求来自本机。所以两件事都得测：本机能用、局域网看不到。
func TestDiagOpenToLocalhostOnly(t *testing.T) {
	_, mux := newTestAdmin(t, false)

	t.Run("局域网直连拿不到", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/diag", nil)
		req.RemoteAddr = "192.168.2.77:40000"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("状态码 = %d，期望 403 —— "+
				"局域网上的扫描者不该知道这个端口后面跑着什么", rec.Code)
		}
	})

	t.Run("本机能用且不泄露任何东西", func(t *testing.T) {
		req := localReq("GET", "/api/diag", "")
		req.Header.Set("Ugreen-Ttk", "a-very-secret-token-value")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（这个接口就是给排查用的）", rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "SUPER-SECRET-PASSWORD") {
			t.Error("诊断接口泄露了 root 密码")
		}
		// token 只能报长度，不能回显原值
		if strings.Contains(body, "a-very-secret-token-value") {
			t.Error("诊断接口把 Ugreen-Ttk 的原值回显出来了，只该报长度")
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
		// 没带网关头，这个信号该是 false —— 正是我们要它告诉我们的
		if resp["gateway_authed"] != false {
			t.Errorf("gateway_authed = %v，没有网关头时应为 false", resp["gateway_authed"])
		}
	})
}

// 管理页面本身也必须设闸。
//
// 早先这里是裸的 FileServer：局域网上任何人 curl http://NAS:23306/ 都能拿到整份
// 管理页 HTML。API 全都调不动没错，但页面骨架已经把"这台 NAS 上跑着 MariaDB、
// 有哪些配置项、连接串长什么样"全交代了 —— 页面结构本身就是信息。
func TestManagementPageBlockedFromLAN(t *testing.T) {
	_, mux := newTestAdmin(t, false)

	for _, path := range []string{"/", "/index.html", "/cloudwindow.js"} {
		t.Run("局域网拿不到"+path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			req.RemoteAddr = "192.168.2.77:40000"
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("状态码 = %d，期望 403", rec.Code)
			}
			body := rec.Body.String()
			// 错误页必须是"什么都不说"的：不出现应用名、端口、任何配置项，
			// 也不能把管理页的任何片段漏出来。
			for _, leak := range []string{
				"MariaDB", "mariadb", "root", "密码", "端口", "3306",
				"局域网访问", "连接信息", "cloudWindow",
			} {
				if strings.Contains(body, leak) {
					t.Errorf("403 页面里出现了 %q —— 它应该什么都不透露：\n%s", leak, body)
				}
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q，浏览器直连时应当回一张 HTML 页面", ct)
			}
		})
	}

	t.Run("本机照常拿得到页面", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, localReq("GET", "/", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200 —— 网关是从本机连过来的，不能把它也挡了", rec.Code)
		}
	})
}

func TestAdminIdentityPassesThrough(t *testing.T) {
	_, mux := newTestAdmin(t, false)
	req := adminReq("GET", "/api/status", "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员访问 /api/status 得到 %d，期望 200\n%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp["root_password"] != "SUPER-SECRET-PASSWORD" {
		t.Errorf("管理员应该能看到 root 密码，实际拿到 %v", resp["root_password"])
	}
}

// identityFrom 只认 Ugreen-User-ID。
// 【不要】把 Ugreen-User-Type 也变成必填 —— 它的取值和文档对不上，
// 万一某类用户根本不带这个头，就会把人无谓地挡在门外。
func TestIdentityFromRequiresOnlyUserID(t *testing.T) {
	cases := []struct {
		name    string
		id, typ string
		wantOK  bool
	}{
		{"齐全", "1", "admin", true},
		{"缺 ID", "", "admin", false},
		{"缺 Type 也应放行", "1", "", true},
		{"都没有", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if c.id != "" {
				r.Header.Set(headerUserID, c.id)
			}
			if c.typ != "" {
				r.Header.Set(headerUserType, c.typ)
			}
			if _, ok := identityFrom(r); ok != c.wantOK {
				t.Errorf("identityFrom ok = %v，期望 %v", ok, c.wantOK)
			}
		})
	}
}

// --dev-no-auth 是本地开发用的后门，必须【只有显式打开时】才生效。
func TestDevNoAuthBypass(t *testing.T) {
	_, mux := newTestAdmin(t, true)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, localReq("GET", "/api/status", ""))
	if rec.Code != http.StatusOK {
		t.Errorf("开了 --dev-no-auth 之后应该放行，得到 %d", rec.Code)
	}
}
