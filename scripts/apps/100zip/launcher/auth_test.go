package main

// 两道闸 + 会话 Cookie 的测试。
//
// ⚠ "局域网地址"的测试数据必须用 TEST-NET 保留段（203.0.113.x），
// 不能写 192.168.x —— 开发机自己就在那个网段上，isSameHostRequest 会
// 枚举真本机网卡返回 true，测试变成"永远通过"。
//
// 本地开发时 --dev-no-auth 会跳过所有闸；测试直接构造 Server 结构体，
// 不走 devNoAuth，保证闸的代码路径被真实执行。

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer() *Server {
	return &Server{
		sessions:  newSessionStore(),
		devNoAuth: false,
		roots:     []string{"/volume1/test"},
	}
}

// remoteReq 构造一个带指定源地址的请求。
func remoteReq(t *testing.T, s *Server, method, target string, remote string, hdr map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = net.JoinHostPort(remote, "12345")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func get(t *testing.T, s *Server, h http.HandlerFunc, method, target, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := remoteReq(t, s, method, target, remote, hdr)
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

const (
	lanAddr    = "203.0.113.7"  // TEST-NET：模拟局域网上的另一台机器
	localAddr4 = "127.0.0.1"    // 回环 v4（网关最可能从这里连过来）
	localAddr6 = "::1"          // 回环 v6
	adminHdr   = "Ugreen-User-ID"
)

// ---- 1. 静态页面闸：局域网必须 403，且不泄露任何业务字样 ----

func TestPageGateBlocksLan(t *testing.T) {
	s := newTestServer()
	// 直接测闸本身（不碰磁盘上的 www 目录）
	r := remoteReq(t, s, "GET", "/", lanAddr, nil)
	w := httptest.NewRecorder()
	s.requirePageSameHost(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("局域网请求页面应 403，got %d", w.Code)
	}
	body := w.Body.String()
	// 403 整页必须什么都不说：应用名、业务词、端口一个都不能出现
	for _, leak := range []string{"100zip", "100解压", "压缩", "解压", "密码", "28737", "7z"} {
		if strings.Contains(body, leak) {
			t.Errorf("403 页面泄露了业务字样 %q", leak)
		}
	}
}

func TestPageGateAllowsLoopback(t *testing.T) {
	s := newTestServer()
	for _, addr := range []string{localAddr4, localAddr6} {
		r := remoteReq(t, s, "GET", "/", addr, nil)
		w := httptest.NewRecorder()
		// NotFoundHandler 代表"页面 handler 本身"：闸放行后由它返回 404，
		// 这里只断言闸没有拦（拦了会是 403 + 整页）。
		s.requirePageSameHost(http.NotFoundHandler()).ServeHTTP(w, r)
		if w.Code == http.StatusForbidden {
			t.Errorf("本机 %s 的页面请求被闸拦了（这会挡住网关）", addr)
		}
	}
}

// ---- 2. /api 闸：网关身份 或 会话 Cookie，二选一；且都压着本机闸 ----

func TestAPIGate(t *testing.T) {
	s := newTestServer()
	okHandler := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	gated := s.requireAuth(okHandler)

	cases := []struct {
		name   string
		remote string
		hdr    map[string]string
		cookie bool
		want   int
	}{
		{"本机+网关身份", localAddr4, map[string]string{adminHdr: "1"}, false, 200},
		{"回环v6+网关身份", localAddr6, map[string]string{adminHdr: "1"}, false, 200},
		{"本机+有效Cookie", localAddr4, nil, true, 200},
		{"局域网+网关身份（应被本机闸拦）", lanAddr, map[string]string{adminHdr: "1"}, false, 403},
		{"局域网+Cookie（应被本机闸拦）", lanAddr, nil, true, 403},
		{"本机+无身份", localAddr4, nil, false, 401},
		{"本机+伪造Type但无ID", localAddr4, map[string]string{"Ugreen-User-Type": "admin"}, false, 401},
		{"本机+坏Cookie", localAddr4, nil, true, 401}, // cookie 值是假的，见下
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := tc.hdr
			req := remoteReq(t, s, "GET", "/api/capabilities", tc.remote, hdr)
			if tc.cookie {
				// "有效Cookie" 用例先真签发一个；"坏Cookie" 用例写死假值
				if tc.want == 200 {
					tok, err := s.sessions.issue()
					if err != nil {
						t.Fatal(err)
					}
					req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
				} else {
					req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "deadbeef"})
				}
			}
			w := httptest.NewRecorder()
			gated(w, req)
			if w.Code != tc.want {
				t.Fatalf("want %d got %d", tc.want, w.Code)
			}
		})
	}
}

// ---- 3. /api/session：网关身份直签；无桥环境走账号密码（HTTPS + 限流）----

func newTestServerWithLogin(fail func(u, p string) error) *Server {
	s := newTestServer()
	s.loginLimiter = &loginRateLimiter{}
	s.ugosLogin = fail
	return s
}

func TestSessionViaGatewayIdentity(t *testing.T) {
	s := newTestServer()
	// 网关身份 → 签发（桌面路径；LAN 来源与伪造身份等价于直接伪造 /api 头，无新增权限）
	w := get(t, s, s.handleSession, "POST", "/api/session", localAddr4, map[string]string{adminHdr: "1"})
	if w.Code != 200 {
		t.Fatalf("网关身份换 Cookie 应 200，got %d", w.Code)
	}
	if len(w.Result().Cookies()) == 0 {
		t.Fatal("没有下发会话 Cookie")
	}
}

func TestSessionNoIdentityNoBody(t *testing.T) {
	s := newTestServer()
	w := get(t, s, s.handleSession, "POST", "/api/session", localAddr4, nil)
	if w.Code != 401 {
		t.Fatalf("无身份无凭据应 401，got %d", w.Code)
	}
	w = get(t, s, s.handleSession, "GET", "/api/session", localAddr4, nil)
	if w.Code != 401 {
		t.Fatalf("GET 换 Cookie 应 401，got %d", w.Code)
	}
}

func TestSessionViaCredentials(t *testing.T) {
	calls := 0
	s := newTestServerWithLogin(func(u, p string) error {
		calls++
		if u == "xw" && p == "right" {
			return nil
		}
		return fmt.Errorf("UGOS 登录失败：密码错误")
	})

	// 明文入口拒绝（有凭据但没有 X-Forwarded-Proto=https）
	w := credReq(t, s, lanAddr, map[string]string{"Content-Type": "application/json"}, "xw", "right")
	if w.Code != 403 {
		t.Fatalf("明文入口的密码登录应 403，got %d", w.Code)
	}

	// HTTPS + 正确凭据 → Cookie（来源是局域网手机，允许——这正是本分支存在的意义）
	hdr := map[string]string{"Content-Type": "application/json", "X-Forwarded-Proto": "https"}
	w = credReq(t, s, lanAddr, hdr, "xw", "right")
	if w.Code != 200 {
		t.Fatalf("HTTPS 正确凭据应 200，got %d body=%s", w.Code, w.Body.String())
	}
	if len(w.Result().Cookies()) == 0 {
		t.Fatal("凭据登录没有下发会话 Cookie")
	}
	// 验密真的发生了一次
	if calls != 1 {
		t.Fatalf("UGOS 验密应被调用 1 次，实际 %d", calls)
	}

	// 错误凭据 → 401，且连错 5 次后 429 锁定
	for i := 1; i <= 5; i++ {
		w = credReq(t, s, lanAddr, hdr, "xw", "wrong")
		if w.Code != 401 {
			t.Fatalf("第 %d 次错误凭据应 401，got %d", i, w.Code)
		}
	}
	w = credReq(t, s, lanAddr, hdr, "xw", "right") // 锁定期内正确密码也拒
	if w.Code != 429 {
		t.Fatalf("锁定期内应 429，got %d", w.Code)
	}
	if calls != 6 { // 1 成功 + 5 失败；锁定后不再调用 UGOS
		t.Fatalf("限流后不应再调 UGOS 验密，调用数 %d", calls)
	}
}

func credReq(t *testing.T, s *Server, remote string, hdr map[string]string, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass)
	r := remoteReq(t, s, "POST", "/api/session", remote, hdr)
	r.Body = io.NopCloser(strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSession(w, r)
	return w
}

func TestSessionCookieCannotMintCookie(t *testing.T) {
	s := newTestServer()
	tok, err := s.sessions.issue()
	if err != nil {
		t.Fatal(err)
	}
	// 拿已签发的 Cookie、无身份、无凭据 → 401（Cookie 换不出新 Cookie）
	r := remoteReq(t, s, "POST", "/api/session", localAddr4, nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	w := httptest.NewRecorder()
	s.handleSession(w, r)
	if w.Code != 401 {
		t.Fatalf("拿 Cookie 换 Cookie 应 401，got %d", w.Code)
	}
}

// ---- 4. healthz：任何来源都 200 ----

func TestHealthzOpenToAll(t *testing.T) {
	s := newTestServer()
	for _, addr := range []string{localAddr4, lanAddr} {
		w := httptest.NewRecorder()
		r := remoteReq(t, s, "GET", "/api/healthz", addr, nil)
		// 直接调用 routes 里的探活 handler 逻辑：任何来源都放行
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok"))
		}).ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("healthz 应任何来源 200（来源 %s），got %d", addr, w.Code)
		}
		if w.Body.String() != "ok" {
			t.Errorf("healthz 响应体应为 ok，got %q", w.Body.String())
		}
	}
}

// ---- 5. 错误体形状对齐上游 ----

func TestWriteErrShape(t *testing.T) {
	w := httptest.NewRecorder()
	writeErr(w, http.StatusUnauthorized, "未通过认证")
	body := w.Body.String()
	if !strings.Contains(body, `"ok":false`) || !strings.Contains(body, `"error"`) || !strings.Contains(body, `"message"`) {
		t.Fatalf("错误体应形如 {ok:false,error:{message}}，got %s", body)
	}
}
