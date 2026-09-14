package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 走一遍真实流程：页面用网关身份换 Cookie，之后浏览器自己发的请求靠 Cookie 过闸。
//
// 这条链路就是"缩略图裂了、二维码出不来"的修复本身 —— <img> 请求
// 加不了任何自定义头，只能靠 Cookie。
func TestSession_ExchangeThenCookieWorks(t *testing.T) {
	s := testServer(t)
	s.proxy = newTestProxy(t, s) // 上游没起来，走到反代就是 503，说明已经过闸了

	rec := do(t, s, http.MethodPost, "/api/session", localAddr, map[string]string{
		headerUserID: "42",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("带网关身份换会话应该 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	var token string
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("没有下发会话 Cookie")
	}

	// 模拟 <img src="/api/local/thumbnail?..."> ：只有 Cookie，没有任何自定义头
	req := httptest.NewRequest(http.MethodGet, "/api/local/thumbnail?path=/x.jpg", nil)
	req.RemoteAddr = localAddr
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec2 := httptest.NewRecorder()
	s.routes().ServeHTTP(rec2, req)

	if rec2.Code == http.StatusUnauthorized {
		t.Fatal("带着有效会话 Cookie 的请求不该被判成未认证 —— 缩略图就是这么裂的")
	}
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("应该已经过闸并走到反代（上游没起来 = 503），实际 %d", rec2.Code)
	}
}

// 会话 Cookie 换不出新的会话 Cookie —— /api/session 只认网关身份。
func TestSession_CookieCannotMintAnotherSession(t *testing.T) {
	s := testServer(t)
	token, err := s.sessions.issue()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/session", nil)
	req.RemoteAddr = localAddr
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("只拿 Cookie 应该换不到新会话，实际 %d", rec.Code)
	}
}

// ⚠ Cookie 不能把"必须来自 NAS 本机"那道闸架空。
// 局域网上的机器就算不知从哪弄到一个有效 token，也必须被挡在外面。
func TestSession_CookieDoesNotBypassSameHostGate(t *testing.T) {
	s := testServer(t)
	token, err := s.sessions.issue()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.RemoteAddr = lanAddr
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("局域网请求即使带着有效 Cookie 也必须 403，实际 %d", rec.Code)
	}
}

func TestSession_RejectsUnknownToken(t *testing.T) {
	s := testServer(t)
	if s.sessions.valid("这不是发出去过的 token") {
		t.Fatal("认了一个没发过的 token")
	}
	if s.sessions.valid("") {
		t.Fatal("空 token 不该被认")
	}
}

// ⚠ inner 应用跑在 UGOS 桌面的跨站 iframe 里：默认 SameSite=Lax 的 Cookie
// 在子请求里根本不会被带上，表现是"换了 Cookie 但图片还是 401"。
// 网关是 HTTPS（带 X-Forwarded-Proto），这时必须发 SameSite=None; Secure。
func TestSession_CookieAttributesFollowProtocol(t *testing.T) {
	s := testServer(t)

	rec := do(t, s, http.MethodPost, "/api/session", localAddr, map[string]string{
		headerUserID:        "42",
		"X-Forwarded-Proto": "https",
	})
	raw := rec.Header().Get("Set-Cookie")
	if !strings.Contains(raw, "SameSite=None") || !strings.Contains(raw, "Secure") {
		t.Fatalf("网关是 HTTPS 时必须 SameSite=None; Secure，实际：%s", raw)
	}
	if !strings.Contains(raw, "HttpOnly") {
		t.Fatalf("会话 Cookie 必须 HttpOnly，实际：%s", raw)
	}

	// 本地开发是明文 HTTP —— 这时发 Secure 的话浏览器根本不会存下来
	rec = do(t, s, http.MethodPost, "/api/session", localAddr, map[string]string{
		headerUserID: "42",
	})
	raw = rec.Header().Get("Set-Cookie")
	if strings.Contains(raw, "Secure") {
		t.Fatalf("明文 HTTP 下不该带 Secure，实际：%s", raw)
	}
	if !strings.Contains(raw, "SameSite=Lax") {
		t.Fatalf("明文 HTTP 下应该是 SameSite=Lax，实际：%s", raw)
	}
}
