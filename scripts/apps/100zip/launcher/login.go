package main

// /api/session 的"账号密码开通"分支 + 登录限流。
//
// 【为什么需要这条分支】inner 应用的 /api 认证靠网关注入 Ugreen-User-*，
// 前提是请求带 Ugreen-Ttk（桌面 JSSDK 取得）。手机浏览器里没有宿主桥，
// 拿不到 Ttk——移动端用户唯一的认证方式就是直接输 NAS 账号密码，
// 由管理壳到本机 UGOS 登录接口验密（见 ugos.go），验过签会话 Cookie。
//
// 【安全边界】
//   - 只接受 HTTPS 入口（X-Forwarded-Proto=https，即从网关进来）：
//     明文 LAN 直连端口输密码会被拒——密码不能裸奔过局域网。
//   - 限流：连续失败 5 次锁 15 分钟（全局计数，单用户 NAS 足够）。
//     这个端点和 NAS 自己的登录页一样对局域网可达，暴露面等价。
//   - 桌面桥路径（网关身份）不经过这里，不受限流影响。
//   - 密码只转发给 UGOS 自己的登录接口验证，不落盘、不写日志。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	loginMaxFails    = 5
	loginLockWindow  = 15 * time.Minute
	loginMaxBodySize = 8 * 1024 // 用户名+密码的 JSON 远用不了这么大
)

type loginRateLimiter struct {
	mu       sync.Mutex
	fails    int
	lockedAt time.Time
}

func (l *loginRateLimiter) blocked() (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails < loginMaxFails {
		return false, 0
	}
	if elapsed := time.Since(l.lockedAt); elapsed < loginLockWindow {
		return true, loginLockWindow - elapsed
	}
	l.fails = 0 // 锁定期满，重新计数
	return false, 0
}

func (l *loginRateLimiter) recordFail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails++
	if l.fails >= loginMaxFails {
		l.lockedAt = time.Now()
	}
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleSession：两条路开会的话 Cookie。
//   - 桌面（有桥）：网关身份即可（原行为，POST 不带 body）。
//   - 无桥环境（手机等）：POST JSON {username,password}，管理壳到 UGOS 验密后签发。
//
// 注意这个端点【不要求】来自本机——手机是局域网客户端，这正是它存在的意义；
// 暴露面与 NAS 自己的登录页等价，靠 HTTPS 强制 + 限流约束。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	// 桌面路径：网关注入了身份，直接签（保持原语义：拿 Cookie 换不出新 Cookie 之外的特权）
	if _, ok := identityFrom(r); ok {
		s.issueSession(w, r)
		return
	}

	if s.devNoAuth {
		s.issueSession(w, r)
		return
	}

	// 无桥路径：POST + JSON 凭据。HTTPS 门卫只在【确实提交了凭据】时拦——
	// 没带凭据的请求走统一的 401 提示，不暴露"这里有登录入口"以外的东西。
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusUnauthorized, authFailureHint)
		return
	}

	var req loginReq
	body := http.MaxBytesReader(w, r.Body, loginMaxBodySize)
	if err := json.NewDecoder(body).Decode(&req); err != nil || (req.Username == "" && req.Password == "") {
		writeErr(w, http.StatusUnauthorized, authFailureHint)
		return
	}
	if !requestIsHTTPS(r) {
		writeErr(w, http.StatusForbidden,
			"账号密码登录只接受 HTTPS 入口（从 NAS 桌面/网关地址打开）。明文端口不接受密码。")
		return
	}
	if blocked, wait := s.loginLimiter.blocked(); blocked {
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("登录失败次数过多，已锁定，请 %d 分钟后再试", int(wait.Minutes())+1))
		return
	}

	if err := s.ugosLogin(req.Username, req.Password); err != nil {
		s.loginLimiter.recordFail()
		log.Printf("[login] UGOS 验密失败（用户 %q）：%v", req.Username, err)
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	log.Printf("[login] UGOS 验密通过，已签发会话（用户 %q，来源 %s）", req.Username, r.RemoteAddr)
	s.issueSession(w, r)
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request) {
	token, err := s.sessions.issue()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "会话签发失败："+err.Error())
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
