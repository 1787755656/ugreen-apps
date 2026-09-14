package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestProxy 指向一个不会有人监听的端口，用来复现"上游没起来"。
func newTestProxy(t *testing.T, s *Server) *httputil.ReverseProxy {
	t.Helper()
	target, _ := url.Parse("http://127.0.0.1:1")
	p := httputil.NewSingleHostReverseProxy(target)
	p.ErrorHandler = s.proxyError
	return p
}

// ⚠ 局域网地址一律用 TEST-NET 保留段（203.0.113.0/24）。
//
// 随手写 192.168.x.x 是不行的：开发机自己可能就在那个网段上，
// isSameHostRequest 会真的枚举到本机网卡而返回 true —— 测试从"验证这道闸"
// 变成"永远通过"，而且恰恰是在最不该失效的那条断言上。
const lanAddr = "203.0.113.5:51234"
const localAddr = "127.0.0.1:51234"

func testServer(t *testing.T) *Server {
	t.Helper()
	p := testPaths(t)
	p.WWW = filepath.Join(t.TempDir(), "www")
	if err := os.MkdirAll(p.WWW, 0o755); err != nil {
		t.Fatal(err)
	}
	// 页面里放上真实页面会有的字样，403 泄露检查才有意义
	page := `<html><title>多网盘下载器</title><body>百度网盘 BDUSS 夸克 Cookie 下载目录 28686</body></html>`
	if err := os.WriteFile(filepath.Join(p.WWW, "index.html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Server{
		paths:       p,
		child:       newSupervisor(p, 28687, filepath.Join(p.Data, "downloads")),
		sessions:    newSessionStore(),
		downloadDir: filepath.Join(p.Data, "downloads"),
	}
}

func do(t *testing.T, s *Server, method, path, remote string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

// 1. 局域网直连静态页面必须 403，且页面里不能出现任何业务字样。
func TestPage_BlockedFromLANAndLeaksNothing(t *testing.T) {
	s := testServer(t)
	rec := do(t, s, http.MethodGet, "/", lanAddr, nil)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("局域网访问页面应该 403，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"多网盘", "百度", "夸克", "BDUSS", "Cookie", "下载目录", "28686", "clouddl"} {
		if strings.Contains(body, leak) {
			t.Errorf("403 页面泄露了 %q", leak)
		}
	}
}

// 2. 本机来的请求照常放行 —— 这条最容易漏。
//
// 安全性测试很容易只测"挡住了没有"，忘了测"该放的放了没有"；
// 而把网关自己也挡掉的后果是应用【彻底打不开】。
func TestPage_AllowedFromLocalhost(t *testing.T) {
	s := testServer(t)
	rec := do(t, s, http.MethodGet, "/", localAddr, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("本机访问页面应该 200，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "多网盘下载器") {
		t.Fatal("本机访问应该拿到真正的页面")
	}
}

// 3. /api/diag：本机 200 且不回显 token 原值；局域网 403。
func TestDiag(t *testing.T) {
	s := testServer(t)

	rec := do(t, s, http.MethodGet, "/api/diag", localAddr, map[string]string{
		"Ugreen-Ttk": "super-secret-token-value",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("本机访问 /api/diag 应该 200，实际 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "super-secret-token-value") {
		t.Fatal("/api/diag 回显了 token 原值")
	}

	rec = do(t, s, http.MethodGet, "/api/diag", lanAddr, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("局域网访问 /api/diag 应该 403，实际 %d", rec.Code)
	}
}

// 4. /api/healthz：任何来源都 200，且响应体只有 ok。
//
// ⚠ 绝不能给它加闸：平台的探测请求不经过网关、也可能不从预期地址来，
//   挡了会让应用一直显示"启动失败"。
func TestHealthz_OpenToEveryone(t *testing.T) {
	s := testServer(t)
	for _, addr := range []string{localAddr, lanAddr} {
		rec := do(t, s, http.MethodGet, "/api/healthz", addr, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 访问 /api/healthz 应该 200，实际 %d", addr, rec.Code)
		}
		if rec.Body.String() != "ok" {
			t.Fatalf("/api/healthz 的响应体应该只有 ok，实际 %q", rec.Body.String())
		}
	}
}

// 5. 业务接口两道闸都要过。
func TestAPI_NeedsBothGates(t *testing.T) {
	s := testServer(t)

	// 局域网 + 伪造的身份头 → 第一道闸挡下
	rec := do(t, s, http.MethodGet, "/api/settings", lanAddr, map[string]string{
		headerUserID:   "1",
		headerUserType: "admin",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("局域网请求（哪怕伪造了身份头）应该 403，实际 %d", rec.Code)
	}

	// 本机但没过网关 → 第二道闸挡下
	rec = do(t, s, http.MethodGet, "/api/settings", localAddr, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("没有网关身份的本机请求应该 401，实际 %d", rec.Code)
	}
	// 401 的话术要指路 /api/diag，否则用户完全无从下手
	if !strings.Contains(rec.Body.String(), "/api/diag") {
		t.Error("401 的提示里应该告诉用户去看 /api/diag")
	}
}

// 6. 两道闸都过、但上游 Python 没起来时，要给一句人话，而不是裸的 502。
func TestAPI_UpstreamDownGivesUsefulMessage(t *testing.T) {
	s := testServer(t)
	// 指向一个没人监听的端口
	s.proxy = newTestProxy(t, s)

	rec := do(t, s, http.MethodGet, "/api/settings", localAddr, map[string]string{
		headerUserID: "1",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("上游没起来时应该 503，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "停止") || !strings.Contains(body, "日志") {
		t.Errorf("503 的话术要能指导用户下一步，实际是：%s", body)
	}
}
