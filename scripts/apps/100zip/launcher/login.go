package main

// /api/session —— 会话 Cookie 的签发入口。
//
// 【为什么需要它】inner 应用的 /api 认证靠网关注入 Ugreen-User-*，前提是
// 请求带 Ugreen-Ttk（桌面 JSSDK 取得）。但界面里有些请求是浏览器自己发起的
// （诊断包导出走 <a> 下载），加不了自定义头 —— 开场先用 token 换一个会话
// Cookie，之后这些请求靠 Cookie 过闸（见 session.go）。
//
// 【只有桌面一条路】本应用 supports 只有 pc（DECISIONS/008）。手机浏览器
// 没有宿主桥、拿不到 Ttk，所以【不再】提供任何"无桥认证"：
// 曾经有过的账号密码分支（含一次性 RSA 公钥通道、UGOS 验密、失败限流）
// 已随移动端支持一起删除。任何不带网关身份的请求一律 401，
// 响应里不暴露"曾经有登录入口"这类信息。

import (
	"net/http"
)

// handleSession 只认网关注入的身份。
//
// 语义刻意做得最窄：拿 Cookie 换不出新 Cookie，没有身份就什么都不发。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := identityFrom(r); ok {
		s.issueSession(w, r)
		return
	}
	if s.devNoAuth {
		s.issueSession(w, r)
		return
	}
	writeErr(w, http.StatusUnauthorized, authFailureHint)
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
