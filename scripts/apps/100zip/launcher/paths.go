package main

// 目录与可执行文件的定位，以及授权目录的解析。
//
// 沙箱里的目录布局（unit 文件里能看到是怎么围出来的）：
//
//	/var/packages/<appid>/            只读，装包解出来的东西都在这
//	  ├── bin/zip100-launcher         本管理壳
//	  ├── bin/100zip                  上游二进制（交叉编译自上游源码）
//	  ├── bin/vendor/7zip/<plat>/7zzs 压缩引擎（按架构各放一份）
//	  ├── service/www/                上游前端（网关 serve 用；这里也留着兜底）
//	  └── www/                        rootfs_common/www 解出来的前端（同上）
//	/var/packages/<appid>/data|cache|log/   这三个才可写
//
// 【注意安装目录是只读的】——所有可写路径都必须落到 data/cache/log 里去。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Paths struct {
	Install string // 安装目录（只读）
	Data    string // 可写数据目录（上游 --data；任务记录/密码库都在这）
	Cache   string // 可写缓存目录
	Log     string // 可写日志目录

	Bin    string // 上游二进制绝对路径
	Engine string // 7zzs 引擎绝对路径（按启动时架构选）
	WWW    string // 前端静态目录

	Tmp string // 交给上游的 TMPDIR —— 沙箱里【没有 /tmp】
}

// envOr 读环境变量，空则回退。
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func resolvePaths() Paths {
	install := os.Getenv("UGAPP_INSTALL_DIR")
	if install == "" {
		// 平台没注入时（本地开发）从可执行文件位置反推：<install>/bin/xxx
		if exe, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				exe = resolved
			}
			install = filepath.Dir(filepath.Dir(exe))
		}
	}

	data := envOr("UGAPP_DATA_DIR", filepath.Join(install, "data"))
	cache := envOr("UGAPP_CACHE_DIR", filepath.Join(install, "cache"))
	logDir := envOr("UGAPP_LOG_DIR", filepath.Join(install, "log"))

	return Paths{
		Install: install,
		Data:    data,
		Cache:   cache,
		Log:     logDir,

		Bin:    envOr("ZIP100_BIN", filepath.Join(install, "bin", "100zip")),
		Engine: resolveEnginePath(install),
		WWW:    filepath.Join(install, "www"),

		Tmp: filepath.Join(cache, "tmp"),
	}
}

// resolveEnginePath 按运行时架构挑引擎。两个架构的 7zzs 都打进包里
//（各自静态链接，约 3-4MB），启动时选自己那份。
//
// ⚠ 必须用 runtime.GOARCH，不能用平台环境变量：真机验证过平台【不注入】
// UGAPP_ARCH 这类变量，靠它判断在真机上永远走默认分支（0.5.18.0001 的
// crash 循环就是它）。管理壳二进制本身按目标架构编译，runtime.GOARCH
// 天然准确，不依赖任何环境注入。
func resolveEnginePath(install string) string {
	plat := "linux-x64"
	if runtime.GOARCH == "arm64" {
		plat = "linux-arm64"
	}
	p := filepath.Join(install, "bin", "vendor", "7zip", plat, "7zzs")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	// 兜底：万一上游换了引擎目录命名，扫 vendor 下任何一份 7zzs
	matches, _ := filepath.Glob(filepath.Join(install, "bin", "vendor", "7zip", "*", "7zzs"))
	if len(matches) > 0 {
		return matches[0]
	}
	return p
}

// ensureDirs 建出所有必须存在的可写目录。
func (p Paths) ensureDirs() error {
	for _, dir := range []string{p.Data, p.Cache, p.Log, p.Tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// resolveRoots 算出"本次运行授权给上游哪些目录"。
//
// 三个来源，按顺序合并去重：
//  1. 安装参数 ZIP_ROOTS（type: path 参数，multi: true，JSON 数组）——
//     用户在应用中心安装/设置对话框里选的目录，选中的同时平台就把它们
//     bind 进了沙箱（path_permissions 表 → unit 的 BindPaths=）。
//  2.UGAPP_SHARED_DIR 下的真实挂载 —— 兜"用户在应用中心「访问路径」里
//     手动加目录"和"参数值没及时注入"两种情况。
//
// ⚠ 装完的第一次启动参数值【必然是空的】（平台先起服务、2~3 秒后才写
// .env），这不是错误，别打 ERROR 日志吓人；重启一次自然就好。
// 所以这里返回的 roots 允许为空 —— 上游 Guard 空根也能跑起来，
// 只是界面上会显示"先授权一个目录"的引导。
func resolveRoots() ([]string, string) {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimRight(strings.TrimSpace(p), "/")
		if p == "" || p == "/" || seen[p] {
			return
		}
		// 平台自己的 @appcache/@appdata/@applog/@appstore 等系统目录不是用户工作目录，
		// 别登记（它们确实可访问，但会污染界面的"已授权目录"列表）。
		for _, seg := range strings.Split(p, "/") {
			if strings.HasPrefix(seg, "@") {
				return
			}
		}
		// 沙箱里真实可打开的才登记 —— 上游 Guard.AddRoot 反正也会拒掉
		// 解析不了的路径，这里先过一遍能让日志更干净。
		if !dirAccessible(p) {
			return
		}
		seen[p] = true
		out = append(out, p)
	}

	notes := []string{}

	// 来源 1：安装参数（multi: true → JSON 数组；没选过 → 字面量 null）
	if raw := os.Getenv("ZIP_ROOTS"); strings.TrimSpace(raw) != "" && strings.TrimSpace(raw) != "null" {
		var dirs []string
		if err := json.Unmarshal([]byte(raw), &dirs); err == nil {
			before := len(out)
			for _, d := range dirs {
				add(d)
			}
			if len(out) == before {
				notes = append(notes, "安装参数里的目录在沙箱里还打不开（刚装/刚改完参数需要重启一次）")
			}
		} else {
			notes = append(notes, "安装参数 ZIP_ROOTS 不是预期的 JSON 数组，已忽略："+raw)
		}
	}

	// 来源 2：shared 目录下真实 bind 进来的挂载（覆盖「访问路径」手动添加）
	for _, p := range discoverSharedMounts() {
		add(p)
	}

	note := strings.Join(notes, "；")
	return out, note
}

// dirAccessible 真的打开一次目录，而不是 Stat ——
// 沙箱里 /volume1 下未授权的共享文件夹【名字可见但打不开】，
// 只看 Stat 会把不能用的目录当成已授权。
func dirAccessible(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
