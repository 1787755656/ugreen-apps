package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func req(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestIsSameHostRequest(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:54321", true},
		{"[::1]:54321", true},
		// 局域网上的另一台机器。inner 应用的端口在局域网上是真的能连上的
		// （2026-08-06 真机实测），所以这一条不是理论问题。
		//
		// ⚠ 这里【必须用 TEST-NET 保留段】，不能随手写个 192.168.x.x：
		// 开发机自己就在 192.168.2.0/24 上，那样写会真的命中本机网卡，
		// 测试变成"永远通过"，反而把这道闸的回归漏掉。
		{"203.0.113.9:44444", false},
		{"198.51.100.4:1234", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := isSameHostRequest(req(c.addr, nil)); got != c.want {
			t.Errorf("isSameHostRequest(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// 第二道闸：必须带着网关注入的用户身份。
// 【只认 Ugreen-User-ID，不拿 Ugreen-User-Type 硬拦】——
// 真机上属于"管理员用户组"的账号拿到的并不是文档写的 admin，
// 按那个字段拦会把合法管理员挡在门外。
func TestUgUserFromOnlyRequiresID(t *testing.T) {
	if _, ok := ugUserFrom(req("127.0.0.1:1", nil)); ok {
		t.Fatal("什么头都没有，不该认为通过了网关认证")
	}
	if _, ok := ugUserFrom(req("127.0.0.1:1", map[string]string{
		"Ugreen-User-Type": "admin"})); ok {
		t.Fatal("只有 Type 没有 ID，不能算认证通过（Type 是可以随便伪造的）")
	}
	u, ok := ugUserFrom(req("127.0.0.1:1", map[string]string{
		"Ugreen-User-ID": "42", "Ugreen-User-Name": "王", "Ugreen-User-Type": "users"}))
	if !ok {
		t.Fatal("有 ID 就该算认证通过，哪怕 Type 不是 admin")
	}
	if u.Name != "王" {
		t.Errorf("Name = %q", u.Name)
	}
}

func TestAuthRequiresBothGates(t *testing.T) {
	a := &App{}
	h := a.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"secret":"database password"}`))
	})

	t.Run("局域网_带伪造的头也进不来", func(t *testing.T) {
		w := httptest.NewRecorder()
		h(w, req("203.0.113.9:1234", map[string]string{
			"Ugreen-User-ID": "1", "Ugreen-User-Type": "admin"}))
		if w.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d", w.Code)
		}
		if strings.Contains(w.Body.String(), "database password") {
			t.Fatal("响应里泄露了受保护的内容")
		}
	})

	t.Run("本机_但没经过网关", func(t *testing.T) {
		w := httptest.NewRecorder()
		h(w, req("127.0.0.1:1234", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", w.Code)
		}
	})

	t.Run("两道都过", func(t *testing.T) {
		w := httptest.NewRecorder()
		h(w, req("127.0.0.1:1234", map[string]string{"Ugreen-User-ID": "42"}))
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
		}
	})
}

// 方法白名单。没有它的话，一个 GET /api/backups/create 就能触发一次全量导出，
// 而 GET 是能被 <img src> 之类的东西无声发出去的。
func TestPostOnlyAndGetOnly(t *testing.T) {
	called := false
	h := postOnly(func(w http.ResponseWriter, r *http.Request) { called = true })

	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/api/backups/create", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应当 405，got %d", w.Code)
	}
	if called {
		t.Fatal("GET 不该真的执行到 handler")
	}
	if w.Header().Get("Allow") != "POST" {
		t.Errorf("应当带 Allow 头，got %q", w.Header().Get("Allow"))
	}

	w = httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/api/backups/create", nil))
	if !called {
		t.Fatal("POST 应当放行")
	}

	g := getOnly(func(w http.ResponseWriter, r *http.Request) {})
	w = httptest.NewRecorder()
	g(w, httptest.NewRequest(http.MethodDelete, "/api/status", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE 应当 405，got %d", w.Code)
	}
}

// 挡掉的那张页面必须【什么都不透露】。局域网上的人扫到这个端口，
// 只应该知道"此处不对外"，不该知道这台 NAS 上跑着 PostgreSQL。
func TestBlockedPageLeaksNothing(t *testing.T) {
	w := httptest.NewRecorder()
	writeBlockedPage(w)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	body := strings.ToLower(w.Body.String())
	for _, leak := range []string{"postgres", "pgsql", "数据库", "绿联", "25432", "5432"} {
		if strings.Contains(body, strings.ToLower(leak)) {
			t.Errorf("403 页面里出现了 %q，等于告诉扫描者这台机器上跑着什么", leak)
		}
	}
	if !strings.Contains(body, "noindex") {
		t.Error("403 页面应当带 noindex")
	}
}

// handleRoot 是唯一一个不鉴权的路径（系统探活直接打端口，不走网关，
// 包进鉴权会让应用一直被判成"未启动"）。它对外必须闭嘴。
func TestHandleRootSaysNothingToStrangers(t *testing.T) {
	a := &App{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	a.handleRoot(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("非本机请求应当 403，got %d", w.Code)
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "postgresql") {
		t.Fatal("对外的响应里不该出现应用名")
	}

	// 本机（探活）照常 200，否则平台会认为应用没起来
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	a.handleRoot(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("本机探活必须 200，got %d", w.Code)
	}
}
