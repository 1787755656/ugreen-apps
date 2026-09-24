package main

// /api/session 的"账号密码开通"分支 + 登录限流。
//
// 【为什么需要这条分支】inner 应用的 /api 认证靠网关注入 Ugreen-User-*，
// 前提是请求带 Ugreen-Ttk（桌面 JSSDK 取得）。手机浏览器里没有宿主桥，
// 拿不到 Ttk——移动端用户唯一的认证方式就是直接输 NAS 账号密码，
// 由管理壳到本机 UGOS 登录接口验密（见 ugos.go），验过签会话 Cookie。
//
// 【安全边界】
//   - HTTPS 入口可以提交明文密码；明文 HTTP 入口一律拒收——密码不能裸奔过局域网。
//     但手机端打开应用恰恰落在 HTTP 入口上（2026-09 真机确认），所以另开
//     【加密通道】：GET /api/session/key 领一次性 RSA 公钥，密码加密成
//     password_enc 提交（见 loginkeys.go）——机制与绿联平台 HTTP 9999 的
//     应用层加密同水位，HTTP 上也收。
//   - 限流：连续失败 5 次锁 15 分钟（全局计数，单用户 NAS 足够）。
//     这个端点和 NAS 自己的登录页一样对局域网可达，暴露面等价。
//   - 桌面桥路径（网关身份）不经过这里，不受限流影响。
//   - 密码（明文或密文解出后）只转发给 UGOS 自己的登录接口验证，不落盘、不写日志。

import (
	"encoding/base64"
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
	Username    string `json:"username"`
	Password    string `json:"password"`
	PasswordEnc string `json:"password_enc"` // base64(RSA-PKCS1v15(password))，配 key_id
	KeyID       string `json:"key_id"`
}

// handleSession：三条路开会话 Cookie。
//   - 桌面（有桥）：网关身份即可（原行为，POST 不带 body）。
//   - 已有有效会话 Cookie：只确认不重签——拿 Cookie 换不出新 Cookie。
//     这条是手机端的生命线：前端每次加载都 POST 探测一次，带着有效 Cookie
//     的探测必须得到 200，否则认证横幅永远弹着（2026-09 手机端死循环根因）。
//   - 无桥环境（手机等）：POST JSON 凭据，管理壳到 UGOS 验密后签发。
//     密码可以是明文（仅 HTTPS 入口）或 password_enc 密文（HTTP 入口唯一合法形态）。
//
// 注意这个端点【不要求】网关认证——无桥环境拿不出 Ttk，这正是它存在的意义。
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

	// 有效会话确认。不重签、不延期（延期是"换出新 Cookie"的变体）。
	if s.sessions.valid(sessionTokenFrom(r)) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": true})
		return
	}

	// 无桥路径：POST + JSON 凭据。没带凭据的请求统一 401，不暴露登录入口的存在性。
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusUnauthorized, authFailureHint)
		return
	}

	var req loginReq
	body := http.MaxBytesReader(w, r.Body, loginMaxBodySize)
	if err := json.NewDecoder(body).Decode(&req); err != nil || (req.Username == "" && req.Password == "" && req.PasswordEnc == "") {
		writeErr(w, http.StatusUnauthorized, authFailureHint)
		return
	}

	// 密码来源：优先密文通道；两者都带以密文为准（明文字段此时视作未使用）。
	password := req.Password
	encrypted := false
	if req.PasswordEnc != "" {
		encrypted = true
		ct, err := base64.StdEncoding.DecodeString(req.PasswordEnc)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "密码密文不是合法的 base64。")
			return
		}
		pt, ok := s.loginKeys.decrypt(req.KeyID, ct)
		if !ok {
			// 伪造/过期/重放的密文按一次失败计——限流器把这类尝试也圈住。
			s.loginLimiter.recordFail()
			writeErr(w, http.StatusUnauthorized, "密码加密通道无效（密钥过期或已使用）。请刷新页面后重新登录。")
			return
		}
		if len(pt) == 0 || len(pt) > 128 {
			writeErr(w, http.StatusUnauthorized, "密码密文解出的内容不合法。")
			return
		}
		password = string(pt)
	}

	// HTTPS 门卫只拦【明文】密码；密文通道在 HTTP 入口放行（机制见 loginkeys.go）。
	if !encrypted && !requestIsHTTPS(r) {
		writeErr(w, http.StatusForbidden,
			"明文密码只接受 HTTPS 入口。HTTP 入口请使用页面上的登录表单（密码会先加密再提交）。")
		return
	}
	if blocked, wait := s.loginLimiter.blocked(); blocked {
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("登录失败次数过多，已锁定，请 %d 分钟后再试", int(wait.Minutes())+1))
		return
	}

	if err := s.ugosLogin(req.Username, password); err != nil {
		s.loginLimiter.recordFail()
		log.Printf("[login] UGOS 验密失败（用户 %q，来源 %s，密文通道 %t）：%v", req.Username, r.RemoteAddr, encrypted, err)
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	log.Printf("[login] UGOS 验密通过，已签发会话（用户 %q，来源 %s，密文通道 %t）", req.Username, r.RemoteAddr, encrypted)
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
