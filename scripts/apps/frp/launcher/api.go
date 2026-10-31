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

	// 登录(开放;带限流 + Origin 同源校验)
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			httpError(w, http.StatusForbidden, "origin 不允许")
			return
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
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    tok,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(sessionTTL.Seconds()),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		log.Printf("登录成功 ip=%s", ip)
	})

	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
			SameSite: http.SameSiteLaxMode, MaxAge: -1,
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
			"shell_version": AppVersion,
			"frp_version":   frpVersion,
			"install_dir":   installDir,
			"data_dir":      dataDir,
			"log_dir":       logDir,
			"remote_addr":   r.RemoteAddr,
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
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ck, err := r.Cookie(sessionCookie)
			if err != nil || !st.validSessionToken(ck.Value) {
				httpError(w, http.StatusUnauthorized, "未登录或会话已过期")
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /api/status", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"shell_version": AppVersion,
			"frp_version":   frpVersion,
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
		if !sameOrigin(r) {
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
		if !sameOrigin(r) {
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
		if !sameOrigin(r) {
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

// sameOrigin 浏览器跨站 POST 防线(配合 SameSite=Lax cookie 双保险):
// 带 Origin 头时必须与 Host 一致;无 Origin 的本机 curl 不拦。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
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
