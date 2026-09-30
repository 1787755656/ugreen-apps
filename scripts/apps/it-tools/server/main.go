// ittools-server —— IT-Tools 静态前端的极简 Go 文件服务器（UGOS Pro 原生沙箱用）
//
// IT-Tools 上游是纯前端 SPA（vue-router history 模式），无后端 API。
// 本服务器只做四件事：探活 /healthz、静态文件、SPA fallback、资源长缓存。
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	port := flag.String("port", "25180", "HTTP listen port")
	if envPort := os.Getenv("PORT"); envPort != "" && *port == "25180" {
		*port = envPort
	}
	flag.Parse()

	www := os.Getenv("ITTOOLS_WWW")
	if www == "" {
		installDir := os.Getenv("UGAPP_INSTALL_DIR")
		if installDir == "" {
			installDir = "."
		}
		www = filepath.Join(installDir, "www")
	}
	wwwAbs, err := filepath.Abs(www)
	if err != nil {
		log.Fatalf("resolve www dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wwwAbs, "index.html")); err != nil {
		log.Fatalf("www/index.html not found under %s: %v", wwwAbs, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", spaHandler(wwwAbs))

	addr := "[::]:" + *port
	log.Printf("it-tools server listening on %s, serving %s", addr, wwwAbs)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// spaHandler：文件存在则原样返回；目录则试 index.html；
// 其余路径（history 路由）回退到 SPA 入口。
// 带扩展名的未知路径回 404 —— 避免把 HTML 当 JS/CSS 返回掩盖构建问题。
func spaHandler(www string) http.Handler {
	indexPath := filepath.Join(www, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := filepath.Clean("/" + r.URL.Path)
		full := filepath.Join(www, upath)
		if full != www && !strings.HasPrefix(full, www+string(filepath.Separator)) {
			http.NotFound(w, r)
			return
		}

		st, err := os.Stat(full)
		if err == nil && st.IsDir() {
			full = filepath.Join(full, "index.html")
			st, err = os.Stat(full)
		}
		if err == nil && !st.IsDir() {
			if strings.HasPrefix(upath, "/assets/") || strings.HasPrefix(upath, "/sw.js") ||
				strings.HasPrefix(upath, "/workbox-") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeFile(w, r, full)
			return
		}

		if strings.Contains(r.URL.Path, ".") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, indexPath)
	})
}
