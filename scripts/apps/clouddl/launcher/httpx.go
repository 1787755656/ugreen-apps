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

// writeErr 的字段名刻意用 detail：上游 FastAPI 的错误体就是 {"detail": "..."}，
// 前端 api.js 读的也是 payload.detail。管理壳自己产生的错误（403/401/503）
// 走同一个形状，前端不用改一行代码就能把话显示出来。
func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"detail": message})
}
