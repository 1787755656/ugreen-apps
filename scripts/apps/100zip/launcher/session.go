package main

// 会话 Cookie —— 让浏览器自己发起的请求也能过闸。
//
// 100zip 的界面里大部分请求都是 fetch（预览也是 fetch 拿 blob），能带上
// Ugreen-Ttk 头。但有两类例外：
//
//	<a> 触发的诊断包导出下载（浏览器直接发起，加不了自定义头）
//	将来任何 <img src="/api/..."> 的用法
//
// 解法照搬 clouddl 的标准做法：页面加载时先用 token 换一个会话 Cookie
// （那一次是 fetch，带得上头），之后浏览器自己发的请求靠 Cookie 过闸。
//
// 安全性没有被削弱：换 Cookie 的那个接口本身要求网关身份，而且所有接口
// （包括这一个）都还压着"必须来自 NAS 本机"那道闸。

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookieName = "zip100_sess"
	sessionTTL        = 12 * time.Hour
	// 上限只是防"页面疯狂刷新把内存撑爆"，正常一个用户就一两条。
	maxSessions = 64
)

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> 过期时刻
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]time.Time{}}
}

func (s *sessionStore) issue() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	if len(s.sessions) >= maxSessions {
		// 满了就整个清掉重来。粗暴但无害：最坏结果是几个标签页各自
		// 重新换一次 Cookie，而这个换取过程对用户是无感的。
		s.sessions = map[string]time.Time{}
	}
	s.sessions[token] = time.Now().Add(sessionTTL)
	return token, nil
}

func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	for known, expiry := range s.sessions {
		// 定长比较，别用 == —— 比较本身不该泄露前缀信息。
		if subtle.ConstantTimeCompare([]byte(known), []byte(token)) == 1 {
			return time.Now().Before(expiry)
		}
	}
	return false
}

func (s *sessionStore) gcLocked() {
	now := time.Now()
	for token, expiry := range s.sessions {
		if now.After(expiry) {
			delete(s.sessions, token)
		}
	}
}

func sessionTokenFrom(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// requestIsHTTPS 判断这条请求最初是不是 https 进来的。
//
// inner 应用的页面由 UGOS 的 HTTPS 网关提供，转发给我们时是明文 HTTP，
// 所以只看 r.TLS 会永远得出 false。网关会带 X-Forwarded-Proto（真机确认）。
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// setSessionCookie 按当前协议自适应 SameSite。
//
// ⚠ inner 应用跑在 UGOS 桌面的【跨站 iframe】里，默认 SameSite=Lax 的 Cookie
// 在子请求里【根本不会被带上】（course-manager 真机踩过：登录页循环进不去）。
// 跨站要用 SameSite=None，而 None 只有配 Secure 才被浏览器接受 ——
// 网关是 HTTPS，所以成立。本地开发走明文 HTTP，那时只能退回 Lax（同站，够用）。
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   int(sessionTTL / time.Second),
	}
	if requestIsHTTPS(r) {
		cookie.SameSite = http.SameSiteNoneMode
		cookie.Secure = true
	} else {
		cookie.SameSite = http.SameSiteLaxMode
	}
	http.SetCookie(w, cookie)
}

// handleSession 用网关身份换会话 Cookie。
//
// 前端（overlay/ugos-auth.js）带着 Ugreen-Ttk 发过来，网关校验通过后注入
// Ugreen-User-*，到这里说明"这个用户确实通过了 UGOS 登录"，发 Cookie。
// 上游 100zip 没有 /api/session 这个路由，所以这个路径在管理壳这里是安全的。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	token, err := s.sessions.issue()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "会话签发失败："+err.Error())
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
