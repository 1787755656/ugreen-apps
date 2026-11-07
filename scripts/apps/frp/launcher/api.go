package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func registerRoutes(mux *http.ServeMux, st *store, frpc, frps *Svc) {
	lim := newLimiter()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("ok"))
	})

	indexHTML, err := wwwFS.ReadFile("www/index.html")
	if err != nil {
		log.Fatalf("内嵌前端缺失: %v", err)
	}
	iconPNG, _ := wwwFS.ReadFile("www/icon.png")

	// tab 直连形态:页面本身不含任何业务数据(配置全走鉴权 API),登录态由前端调
	// /api/status 判定;故 / 与 /favicon.ico 不设闸,401 时前端渲染登录框。
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(iconPNG)
	})
	// inner 形态下页面由网关从 www/ 静态提供,浏览器按 /icon.png 取图标
	mux.HandleFunc("GET /icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(iconPNG)
	})

	// 登录(开放;带限流 + Origin 同源校验)
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		// 登录这一步不卡 Origin:inner 形态下请求经网关转发,Host 可能被改写,
		// 卡死会导致整个应用打不开;而"登录 CSRF"要奏效也得攻击者先知道密码
		// (那还不如自己登),代价与收益不成比例。写接口另由 writeGuard 卡(令牌)。
		if !sameOrigin(r) {
			log.Printf("登录请求 Origin 与 Host 不一致(疑网关改写 Host):origin=%q host=%q,放行",
				r.Header.Get("Origin"), r.Host)
		}
		var req struct {
			Password string `json:"password"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !lim.allow(ip) {
			httpError(w, http.StatusTooManyRequests, "失败次数过多,请 1 分钟后再试")
			return
		}
		if !st.checkPassword(req.Password) {
			lim.fail(ip)
			log.Printf("登录失败 ip=%s", ip)
			httpError(w, http.StatusUnauthorized, "密码错误")
			return
		}
		lim.reset(ip)
		tok, err := st.newSessionToken()
		if err != nil {
			httpError(w, http.StatusInternalServerError, "生成会话失败")
			return
		}
		sameSite, secure := cookieAttrs(r)
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    tok,
			Path:     "/",
			HttpOnly: true,
			SameSite: sameSite,
			Secure:   secure,
			MaxAge:   int(sessionTTL.Seconds()),
		})
		// token 同时回给前端:inner 形态下页面在跨站 iframe 里,网关会把
		// Set-Cookie 压成 SameSite=Strict(PC 客户端还整体拒收第三方 Cookie),
		// 只靠 Cookie 必然登录回环;前端把它存起来走 Authorization 头。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                     true,
			"token":                  tok,
			"using_default_password": st.usingDefaultPassword(),
		})
		log.Printf("登录成功 ip=%s", ip)
	})

	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		sameSite, secure := cookieAttrs(r)
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
			SameSite: sameSite, Secure: secure, MaxAge: -1,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// 自查接口(标准做法:不要求会话,但只对 NAS 本机开放)
	mux.HandleFunc("GET /api/diag", func(w http.ResponseWriter, r *http.Request) {
		if !isSameHost(r) {
			httpError(w, http.StatusForbidden, "diag 只对本机开放")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"shell_version":      AppVersion,
			"frp_version":        frpVersion,
			"install_dir":        installDir,
			"data_dir":           dataDir,
			"log_dir":            logDir,
			"remote_addr":        r.RemoteAddr,
			"has_ugreen_headers": r.Header.Get("Ugreen-User-Id") != "",
			"config_files": map[string]bool{
				"frpc.toml": fileExists(filepath.Join(dataDir, "frpc.toml")),
				"frps.toml": fileExists(filepath.Join(dataDir, "frps.toml")),
			},
			"bins": map[string]bool{
				"frpc": fileExists(filepath.Join(binDir, "frpc")),
				"frps": fileExists(filepath.Join(binDir, "frps")),
			},
		})
	})

	// ---- 以下全部要会话 ----
	// 令牌来源:Cookie(直连 28600 / 同源网页)或 Authorization 头(inner 网关
	// 与 PC 客户端 iframe 里 Cookie 不可用时的主通道)。
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !st.validSessionToken(sessionToken(r)) {
				httpError(w, http.StatusUnauthorized, "未登录或会话已过期")
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /api/status", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"shell_version":          AppVersion,
			"frp_version":            frpVersion,
			"using_default_password": st.usingDefaultPassword(),
			"services": map[string]SvcStatus{
				"frpc": frpc.snapshot(),
				"frps": frps.snapshot(),
			},
		})
	}))

	mux.HandleFunc("GET /api/config", authed(func(w http.ResponseWriter, r *http.Request) {
		svc := r.URL.Query().Get("svc")
		if svc != "frpc" && svc != "frps" {
			httpError(w, http.StatusBadRequest, "svc 必须是 frpc 或 frps")
			return
		}
		p := filepath.Join(dataDir, svc+".toml")
		b, err := os.ReadFile(p)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "读取配置失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"svc": svc, "path": p, "content": string(b),
		})
	}))

	mux.HandleFunc("POST /api/config", authed(func(w http.ResponseWriter, r *http.Request) {
		if !writeGuard(r, st) {
			httpError(w, http.StatusForbidden, "origin 不允许")
			return
		}
		var req struct {
			Svc     string `json:"svc"`
			Content string `json:"content"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.Svc != "frpc" && req.Svc != "frps" {
			httpError(w, http.StatusBadRequest, "svc 必须是 frpc 或 frps")
			return
		}
		if strings.TrimSpace(req.Content) == "" {
			httpError(w, http.StatusBadRequest, "配置内容不能为空")
			return
		}
		p := filepath.Join(dataDir, req.Svc+".toml")
		if err := atomicWrite(p, []byte(req.Content), 0o600); err != nil {
			httpError(w, http.StatusInternalServerError, "写入配置失败: "+err.Error())
			return
		}
		log.Printf("[%s] 配置已保存(%d 字节)", req.Svc, len(req.Content))
		ok, out := verifyConfig(req.Svc)
		writeJSON(w, http.StatusOK, map[string]any{
			"written": true, "verify_ok": ok, "verify_out": out,
		})
	}))

	mux.HandleFunc("POST /api/service", authed(func(w http.ResponseWriter, r *http.Request) {
		if !writeGuard(r, st) {
			httpError(w, http.StatusForbidden, "origin 不允许")
			return
		}
		var req struct {
			Svc    string `json:"svc"`
			Action string `json:"action"` // enable / disable / restart
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.Svc != "frpc" && req.Svc != "frps" {
			httpError(w, http.StatusBadRequest, "svc 必须是 frpc 或 frps")
			return
		}
		target := map[string]*Svc{"frpc": frpc, "frps": frps}[req.Svc]
		switch req.Action {
		case "enable":
			writeDefaultConfigs(false) // 缺失的模板成对补齐,token 保持一致
			st.setEnabled(req.Svc, true)
			target.setEnabled(true)
		case "disable":
			st.setEnabled(req.Svc, false)
			target.setEnabled(false)
		case "restart":
			if !st.enabledFlag(req.Svc) {
				httpError(w, http.StatusBadRequest, "服务未启用,请先启用")
				return
			}
			// 重启前先校验,失败则不动
			if ok, out := verifyConfig(req.Svc); !ok {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"restarted": false, "verify_ok": false, "verify_out": out,
				})
				return
			}
			target.restart()
		default:
			httpError(w, http.StatusBadRequest, "action 必须是 enable/disable/restart")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": target.snapshot()})
	}))

	mux.HandleFunc("GET /api/log", authed(func(w http.ResponseWriter, r *http.Request) {
		svc := r.URL.Query().Get("svc")
		if svc != "frpc" && svc != "frps" {
			httpError(w, http.StatusBadRequest, "svc 必须是 frpc 或 frps")
			return
		}
		lines := 200
		if n, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && n >= 1 && n <= 1000 {
			lines = n
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"svc":   svc,
			"lines": tailLines(filepath.Join(logDir, svc+".log"), lines),
		})
	}))

	mux.HandleFunc("POST /api/password", authed(func(w http.ResponseWriter, r *http.Request) {
		if !writeGuard(r, st) {
			httpError(w, http.StatusForbidden, "origin 不允许")
			return
		}
		var req struct {
			OldPassword string `json:"old_password"`
			NewPassword string `json:"new_password"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if !st.checkPassword(req.OldPassword) {
			httpError(w, http.StatusUnauthorized, "当前密码错误")
			return
		}
		if len(req.NewPassword) < 8 {
			httpError(w, http.StatusBadRequest, "新密码至少 8 位")
			return
		}
		if len(req.NewPassword) > 128 {
			httpError(w, http.StatusBadRequest, "新密码过长")
			return
		}
		if err := st.setPassword(req.NewPassword); err != nil {
			httpError(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		log.Printf("管理密码已修改")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
}

// enabledFlag 独立小包装(避免 store.mu 重入)
func (s *store) enabledFlag(name string) bool {
	return s.frpcEnabled() && name == "frpc" || s.frpsEnabled() && name == "frps"
}

// headerToken 只取请求头里的令牌(不碰 Cookie)
func headerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")); tok != "" {
			return tok
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Session-Token"))
}

// sessionToken 取请求携带的会话令牌:Cookie 优先,其次 Authorization: Bearer,
// 最后 X-Session-Token。inner 形态前端一律走 Authorization。
func sessionToken(r *http.Request) string {
	if ck, err := r.Cookie(sessionCookie); err == nil && ck.Value != "" {
		return ck.Value
	}
	return headerToken(r)
}

// cookieAttrs 按入口协议决定 Cookie 属性:
// 网关走 HTTPS(inner / 反代)时必须是 SameSite=None + Secure,否则跨站 iframe 里
// 的子请求带不上 Cookie;本地明文直连退回 Lax(写死任一个都会出现"登录了但一直 401")。
func cookieAttrs(r *http.Request) (http.SameSite, bool) {
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return http.SameSiteNoneMode, true
	}
	return http.SameSiteLaxMode, false
}

// writeGuard 写接口防线:Origin 与 Host 一致即放行(同源网页);
// 不一致(网关改写 Host / 跨站)时只认请求头里的会话令牌,不认 Cookie ——
// Cookie 在 SameSite=None 下会被跨站请求带上,认它等于把 CSRF 防护废掉。
func writeGuard(r *http.Request, st *store) bool {
	if sameOrigin(r) {
		return true
	}
	tok := headerToken(r)
	return tok != "" && st.validSessionToken(tok)
}

// sameOrigin 浏览器跨站 POST 防线:带 Origin 头时其主机名必须与 Host/
// X-Forwarded-Host 之一相同(端口不参与比较,网关转发时端口会被改写)。
// 无 Origin 的本机 curl 不拦。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return false
	}
	want := u.Hostname()
	for _, h := range []string{r.Host, r.Header.Get("X-Forwarded-Host")} {
		if h == "" {
			continue
		}
		hh := h
		if host, _, err := net.SplitHostPort(h); err == nil {
			hh = host
		}
		if strings.EqualFold(hh, want) {
			return true
		}
	}
	return false
}

func isSameHost(r *http.Request) bool {
	ipStr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ipStr = r.RemoteAddr
	}
	ip := net.ParseIP(ipStr)
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

// ---------- http 小工具 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// readJSON 限制请求体 4MB,解析失败自动回 400
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return false
	}
	return true
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
