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
	} else {
		// EvalSymlinks 失败时退回原始路径，不要致命退出：某些环境下
		// /proc/self/exe 已是被解析的真实路径，或安装目录含无法完全解析的
		// 符号链接；os.Executable() 拿到的路径通常已经可用，强行 Fatalf 反而
		// 会让三个平台 API 服务一起起不来（2026-10-22 真机排查）。
		log.Printf("[api-shim] EvalSymlinks 失败，退回原始路径 %s：%v", exe, err)
	}
	// 安装根可能不在 service/api/ 下：UGOS 安装后 api 二进制经符号链接落到
	// bin/deenmusic，os.Executable() 解析后是 .../bin/deenmusic。这里向上查找
	// 同时含 service/api 与 runtime 的目录作为安装根，与二进制实际位置无关。
	dir := filepath.Dir(exe)
	for {
		if isInstallRoot(dir) {
			return layout{install: dir, service: filepath.Join(dir, "service")}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	log.Fatalf("[api-shim] 找不到安装根（需同时含 service/api 与 runtime）：%s", exe)
	return layout{}
}

// isInstallRoot 判断目录是否为安装根：必须同时含 service/api（三个 API 二进制）
// 与 runtime（自带的 Node / CPython）。
func isInstallRoot(dir string) bool {
	if fi, e := os.Stat(filepath.Join(dir, "service", "api")); e != nil || !fi.IsDir() {
		return false
	}
	if fi, e := os.Stat(filepath.Join(dir, "runtime")); e != nil || !fi.IsDir() {
		return false
	}
	return true
}

func main() {
	log.SetFlags(log.LstdFlags)
	// 角色由启动时的 argv[0] 决定（bridge 用 service/api/<固定文件名> 调用），
	// 不要用 os.Executable()：UGOS 安装后 api 二进制经符号链接落到 bin/deenmusic，
	// os.Executable() 解析符号链接后会得到 "deenmusic"，导致三个服务全进 default
	// 分支致命退出（2026-10-22 真机：全部报"不认识的文件名 deenmusic"）。
	name := filepath.Base(os.Args[0])
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

// fileExists 判断路径是否为存在的普通文件（符号链接跟随）。
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// jsonEscape 转义要嵌进 JSON 字符串字面量里的路径。
//
// 安装/数据目录里出现引号或反斜杠的概率极低，但真出现时拼出来的 JSON 会解析
// 失败，而上游对 JSON 解析失败的处理是"SettingsError 直接起不来"——不是能
// 忽略的小错，所以这里老实转义。
func jsonEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
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

	// 确保临时目录存在：上游 ncm/kugou 启动时会往 os.tmpdir() 写文件
	// （anonymous_token / 缓存 / 解压），而沙箱里 TMPDIR 指向的目录可能还没建。
	// 目录不存在会让 node 启动即抛 ENOENT 退出 1。
	if td := os.TempDir(); td != "" {
		if mkErr := os.MkdirAll(td, 0o755); mkErr != nil {
			log.Printf("[api-shim] 创建临时目录 %s 失败（不影响继续尝试）：%v", td, mkErr)
		}
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
//
// ⚠⚠ 但光有 cwd 不够（2026-10-19 真机 0002 实测）：上游把**相对**路径
//   `web/data/{device.json,credentials.sqlite3,logs/app.log}` 用
//   `PROJECT_ROOT`（= 源码目录 = 只读安装目录）拼成绝对路径，于是
//   `PermissionError: [Errno 13] ... /service/apiqq/web/data`，
//   Application startup failed → QQ 音乐 API 起不来。
//   这里用环境变量（pydantic-settings：QQMUSIC_ 前缀 + `_` 分隔嵌套）把三项
//   覆盖成可写目录下的绝对路径。绝对路径不会再被 PROJECT_ROOT 拼接。
func runPython(l layout) {
	pythonHome := filepath.Join(l.install, "runtime", "python")
	python := filepath.Join(pythonHome, "bin", "python3.11")
	appDir := filepath.Join(l.service, "apiqq")
	// ⚠ 入口不是上游的 web/run.py，而是包内自己的垫片（构建时从 overlay/ 拷进来）：
	// 沙箱里 /dev 只读 → /dev/shm 只读 → loguru 的 enqueue=True 建不出信号量，
	// 上游 run.py 在 logger.add() 就抛 OSError（2026-10-22 真机 0012）。
	// 垫片只把 enqueue 掰成 False，再 runpy 跑上游入口；上游文件原样不动。
	entry := filepath.Join(appDir, "qqapi_boot.py")
	mustExist(python, entry)

	// cwd 由 bridge 设成 <data>/qqmusic；万一没设（或不可写），退回临时目录。
	cwd, err := os.Getwd()
	if err != nil {
		cwd = os.TempDir()
	}
	// 确保临时目录存在（QQ 的 pydantic-settings / python tempfile 也可能用到）。
	if td := os.TempDir(); td != "" {
		if mkErr := os.MkdirAll(td, 0o755); mkErr != nil {
			log.Printf("[api-shim] 创建临时目录 %s 失败（不影响继续尝试）：%v", td, mkErr)
		}
	}
	dataDir := filepath.Join(cwd, "web", "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err != nil {
		dataDir = filepath.Join(os.TempDir(), "qqmusic", "web", "data")
		_ = os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755)
	}

	env := withEnv(os.Environ(),
		"PYTHONHOME="+pythonHome,
		"PYTHONPATH="+filepath.Join(appDir, "vendor")+string(os.PathListSeparator)+appDir,
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONUNBUFFERED=1",
		"PYTHONIOENCODING=utf-8",
		// 把上游硬拼到只读安装目录的三条路径挪到可写目录。
		//
		// ⚠⚠ 命名不是"层级 + 下划线"就能猜出来的：上游用 pydantic-settings 的
		// env_nested_delimiter="_"，它把环境变量名按 "_" **逐段当作字段名** 解析。
		//   QQMUSIC_CREDENTIAL_STORE_PATH → credential / store / path
		//       （credential、store、path 三个都是真字段名 → 生效，这条路一直是对的）
		//   QQMUSIC_CLIENT_DEVICE_PATH    → client / device / path
		//       （ClientConfig 里没有 "device" 字段，只有 "device_path" → 整条被丢弃！
		//        于是回落到默认相对路径，PROJECT_ROOT 又是只读安装目录 →
		//        `FileNotFoundError: .../service/apiqq/web/data/device.json`，
		//        device.json 写不进去 → ensure_session 必炸 → 二维码不出来、
		//        cookie 登录测试也失败。2026-10-22 真机 0010 实测）
		//   QQMUSIC_LOGGING_FILE_PATH 同理（LogConfig.file_path）→ 同样是死键。
		//
		// 结论：字段名里带下划线的嵌套配置**只能**用 JSON 整段覆盖（复杂字段的
		// env 值会被 json.loads 解析）；只有每一段都恰好是字段名时才走扁平写法。
		"QQMUSIC_CREDENTIAL_STORE_PATH=" + filepath.Join(dataDir, "credentials.sqlite3"),
		`QQMUSIC_CLIENT={"device_path":"`+jsonEscape(filepath.Join(dataDir, "device.json"))+`"}`,
		`QQMUSIC_LOGGING={"file_path":"`+jsonEscape(filepath.Join(dataDir, "logs", "app.log"))+`"}`,
	)

	// CA 证书：沙箱里没有 /etc/ssl，OpenSSL 默认信任库为空。上游 QQ 音乐 API
	// 走 niquests，CA 由 wassima 从系统信任库取 —— 取不到就带着空 CA 校验，
	// 所有 https 请求在 ~50ms 内失败（2026-10-22 真机：二维码不显示、榜单空）。
	// 管理壳通常已经把这三个变量传下来了，这里再兜一次，避免单独拉起时失效。
	if ca := filepath.Join(l.service, "vendor", "certifi", "cacert.pem"); fileExists(ca) {
		env = withEnv(env,
			"SSL_CERT_FILE="+ca,
			"REQUESTS_CA_BUNDLE="+ca,
			"CURL_CA_BUNDLE="+ca,
		)
	}

	fmt.Printf("[api-shim] QQ 音乐: exec %s %s (cwd=%s, data=%s)\n", python, entry, cwd, dataDir)

	// 直接 exec：pid 不变，bridge 的 terminate()/poll() 照常作用在真正的
	// Python 进程上；多套一层 fork 会让它杀不到真正的服务。
	if err := syscall.Exec(python, []string{python, entry}, env); err != nil {
		fmt.Fprintf(os.Stderr, "[api-shim] exec %s 失败：%v（QQ 音乐）\n", python, err)
		os.Exit(1)
	}
}
