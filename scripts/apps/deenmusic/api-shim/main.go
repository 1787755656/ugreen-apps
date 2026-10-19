package main

// arm64 上替身三个平台 API 服务的"同名可执行文件"（一次编译、按文件名分发）。
//
// 上游内置的三个 API 服务里，网易云和酷狗是 Node（pkg 打包），QQ 音乐是
// PyInstaller 出来的二进制 —— 三个都是 x86-64，且三个上游仓库都不发 arm64。
// arm64 包的做法是把它们换成"源码 + 包内自带的 Node/CPython 运行时"，
// 而 bridge 的启动契约是死的：
//
//	exec <install>/api/<写死的文件名>     （不带参数）
//	监听地址走环境变量（PORT/HOST、QQMUSIC_SERVER_HOST/PORT）
//	就绪判定由 bridge 自己 probe http://127.0.0.1:<port>
//
// 沙箱里没有 /bin/sh（也没声明 EXEC 权限），所以不能用 shell 脚本顶替；
// 这个几十行的 Go 程序就是那个"同名文件"，负责把环境摆好再 exec 真正的进程。
// 好处：上游代码一行不动，arm64 与 amd64 的 service/api 目录结构完全一致。

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type layout struct {
	install string
	service string
}

func resolveLayout() layout {
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("[api-shim] 无法定位自身路径：%v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// <install>/service/api/<文件名>
	apiDir := filepath.Dir(exe)          // service/api
	service := filepath.Dir(apiDir)      // service
	return layout{install: filepath.Dir(service), service: service}
}

func main() {
	log.SetFlags(log.LstdFlags)
	exe, _ := os.Executable()
	name := filepath.Base(exe)
	l := resolveLayout()

	switch name {
	case "ncm-api-linux-x64":
		runNode(l, "ncm", "网易云音乐")
	case "kugou_api_linux":
		runNode(l, "kugou", "酷狗音乐")
	case "qqmusic-api-linux-x64":
		runPython(l)
	default:
		log.Fatalf("[api-shim] 不认识的文件名 %q（只支持 ncm-api-linux-x64 / kugou_api_linux / qqmusic-api-linux-x64）", name)
	}
}

func mustExist(paths ...string) {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			log.Fatalf("[api-shim] 缺少必需文件 %s：%v", p, err)
		}
	}
}

// withEnv 覆盖式设置环境变量。
//
// ⚠ 必须先把同名项删掉再追加：execve 传的是 char**，重复的 KEY 里
// 优先被读到的是前面那个 —— 实测（2026-10-19 真机）QQ API 就是这样
// `ModuleNotFoundError: No module named 'uvicorn'`：管理壳给主进程设的
// PYTHONPATH 排在前面，把这里的遮蔽掉了。
func withEnv(env []string, kv ...string) []string {
	out := env
	for _, entry := range kv {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		prefix := key + "="
		filtered := out[:0]
		for _, e := range out {
			if !strings.HasPrefix(e, prefix) {
				filtered = append(filtered, e)
			}
		}
		out = append(filtered, entry)
	}
	return out
}

// runNode 起一个 Node 版 API 服务。
//
// cwd 设成应用目录：这些项目用 __dirname 找资源，但个别依赖会按相对路径读
// 文件；pkg 打包成单文件时 cwd 无所谓，从源码跑就必须待在项目根。
// 上游 bridge 传进来的 cwd 是 api 目录（只读、且不是项目根），所以这里改。
func runNode(l layout, app, label string) {
	node := filepath.Join(l.install, "runtime", "node", "bin", "node")
	appDir := filepath.Join(l.service, "apinode", app)
	entry := filepath.Join(appDir, "app.js")
	mustExist(node, entry)

	if err := os.Chdir(appDir); err != nil {
		log.Fatalf("[api-shim] 进入 %s 失败：%v（%s）", appDir, err, label)
	}

	env := withEnv(os.Environ(),
		"NODE_ENV=production",
		// 缓存/临时文件别落到只读的安装目录
		"NPM_CONFIG_CACHE="+filepath.Join(os.TempDir(), "npm-cache"),
	)

	fmt.Printf("[api-shim] %s: exec %s %s (cwd=%s)\n", label, node, entry, appDir)
	if err := syscall.Exec(node, []string{node, entry}, env); err != nil {
		fmt.Fprintf(os.Stderr, "[api-shim] exec %s 失败：%v（%s）\n", node, err, label)
		os.Exit(1)
	}
}

// runPython 起 QQ 音乐 API（纯 Python 项目，跑在包内自带的 CPython 上）。
//
// ⚠ 不改 cwd：bridge 会把 cwd 指到 <data>/qqmusic 并把 config.toml 复制过去，
//   上游 run.py 就是在 cwd 里读配置、建 web/data 的。
func runPython(l layout) {
	pythonHome := filepath.Join(l.install, "runtime", "python")
	python := filepath.Join(pythonHome, "bin", "python3.11")
	appDir := filepath.Join(l.service, "apiqq")
	entry := filepath.Join(appDir, "web", "run.py")
	mustExist(python, entry)

	env := withEnv(os.Environ(),
		"PYTHONHOME="+pythonHome,
		"PYTHONPATH="+filepath.Join(appDir, "vendor")+string(os.PathListSeparator)+appDir,
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONUNBUFFERED=1",
		"PYTHONIOENCODING=utf-8",
	)

	fmt.Printf("[api-shim] QQ 音乐: exec %s %s\n", python, entry)

	// 直接 exec：pid 不变，bridge 的 terminate()/poll() 照常作用在真正的
	// Python 进程上；多套一层 fork 会让它杀不到真正的服务。
	if err := syscall.Exec(python, []string{python, entry}, env); err != nil {
		fmt.Fprintf(os.Stderr, "[api-shim] exec %s 失败：%v（QQ 音乐）\n", python, err)
		os.Exit(1)
	}
}
