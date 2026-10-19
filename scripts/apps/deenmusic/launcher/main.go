package main

// Deen 音乐下载器 —— UGOS Pro 原生应用的管理壳。
//
// 上游：https://github.com/chongya369/music-downloader（作者 chongya369）。
// 本工程【没有改上游的业务代码】，适配全部集中在：
//
//	launcher/                 这个 Go 管理壳（环境、守护、目录）
//	service/webapp/ugos_main.py   一个只做"挂到 /api 前缀 + 换监听地址"的入口层
//	service/api/              三个平台的 API 服务（amd64 用上游二进制，arm64 用源码重建）
//
// 请求路径（inner 模型）：
//
//	浏览器 ──> UGOS 网关 ──/api/*──> Flask(:25400, 仅 127.0.0.1)
//	               └──其余路径──> 网关自己 serve 的 www/（只有一个跳转页）

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

var version = "dev"

func main() {
	port := flag.Int("port", 25400, "Flask 监听端口，必须和 project.yaml 的 port 一致")
	flag.Parse()

	// 日志直接打 stdout —— 平台 append 到
	// /volume1/@appstore/<appid>/log/<appid>.log，journalctl 里没有这些内容。
	log.SetFlags(log.LstdFlags)
	log.Printf("[launcher] Deen 音乐下载器管理壳 %s 启动，端口 %d", version, *port)

	paths := resolvePaths()
	if err := paths.ensureDirs(); err != nil {
		log.Fatalf("[launcher] 建立数据目录失败：%v", err)
	}
	log.Printf("[launcher] 安装目录 %s，数据目录 %s", paths.Install, paths.Data)

	pruneStaleTemp(paths.Tmp)

	download, note := resolveDownloadDir(paths)
	if note != "" {
		log.Printf("[launcher] 下载目录：%s", note)
	}
	log.Printf("[launcher] 本次使用的下载目录：%s", download)

	child := newSupervisor(paths, *port, download)
	go child.Run()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	s := <-sig
	log.Printf("[launcher] 收到 %v，正在停止", s)
	child.Stop()
	log.Print("[launcher] 已退出")
}

// resolveDownloadDir 决定本次的下载目录。
//
// 优先级：安装参数（DEENMUSIC_DOWNLOAD_DIR）> 数据目录下的 downloads。
// 装在参数里的目录要真写一下才算数：装完的第一次启动它还没被 bind 进沙箱，
// 那时它要么不存在、要么不可写。这时退回数据目录，保证应用能起来，
// 用户重启一次（或在应用设置里改）就用上真目录。
func resolveDownloadDir(p Paths) (string, string) {
	param := os.Getenv("DEENMUSIC_DOWNLOAD_DIR")
	fallback := filepath.Join(p.Data, "downloads")

	if param != "" {
		if dirWritable(param) {
			return param, ""
		}
		// 不致命，也不覆盖参数：可能只是这次还没 bind 进来。
		if err := os.MkdirAll(fallback, 0o755); err == nil {
			return fallback, "安装参数指定的目录当前不可写（首次启动尚未授权），先落到数据目录；重启一次即可生效"
		}
		return param, "安装参数指定的目录不可写，且无法创建回退目录，将直接使用该路径"
	}

	if err := os.MkdirAll(fallback, 0o755); err != nil {
		log.Printf("[launcher] 创建默认下载目录失败：%v", err)
	}
	return fallback, "未设置安装参数，使用应用数据目录下的 downloads"
}
