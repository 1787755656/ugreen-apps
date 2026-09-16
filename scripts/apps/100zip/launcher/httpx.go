package main

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeErr 的字段名刻意对齐上游 100zip 的错误形状 {"ok":false,"error":{...}}。
// 上游前端 app.js 的 api() helper 读的是 body.error.{message,hint}，
// 管理壳自己产生的错误（403/401/502）走同一个形状，前端不用改一行代码
// 就能把话显示出来。
func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"ok":    false,
		"error": map[string]any{"code": "LAUNCHER", "message": message, "hint": ""},
	})
}
