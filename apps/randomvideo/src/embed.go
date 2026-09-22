package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
)

//go:embed web
var webFS embed.FS

// staticHandler 提供编译期内嵌的前端。单页应用，没有路由 fallback 的需求。
func staticHandler(lg *log.Logger) http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		lg.Printf("致命：内嵌前端目录不可用: %v", err)
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}
