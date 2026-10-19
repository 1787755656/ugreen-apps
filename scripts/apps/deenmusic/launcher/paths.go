package main

// 目录与可执行文件的定位。
//
// 沙箱里的布局（unit 里能看到怎么围出来的）：
//
//	/var/packages/com.personal.deenmusic/     只读，装包解出来的东西都在这
//	  ├── bin/deenmusic                       本管理壳
//	  ├── runtime/python/                     自带的 CPython 3.11（python-build-standalone）
//	  ├── runtime/node/                       （仅 arm64）自带的 Node 20，跑两个 Node 版 API 服务
//	  ├── service/                            上游项目根（core/ webapp/ api/ vendor/）
//	  │   ├── webapp/ugos_main.py             唯一的适配层入口
//	  │   ├── api/                            三个平台的 API 服务（amd64 是上游二进制，arm64 是 pkg 构建/shim）
//	  │   └── vendor/                         上游 Python 依赖
//	  └── www/                                网关直接 serve 的静态页（只是一个跳转页）
//	/var/packages/<appid>/data|cache|log/     这三个才可写
//
// 【安装目录是只读的】—— 上游"配置写在程序目录"的假设在这里不成立，
// 所有可写路径都必须落到 data/cache/log 里。

import (
	"os"
	"path/filepath"
)

type Paths struct {
	Install string // 安装目录（只读）
	Data    string // 可写数据目录
	Cache   string // 可写缓存目录
	Log     string // 可写日志目录

	Python  string // 解释器绝对路径
	PyHome  string // PYTHONHOME
	Vendor  string // PYTHONPATH 里的第三方依赖目录
	Service string // 上游项目根（_ROOT）
	Entry   string // 入口脚本 ugos_main.py
	APIDir  string // 三个平台 API 服务所在目录（上游根/api）

	Tmp string // TMPDIR —— 沙箱里【没有 /tmp】
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func resolvePaths() Paths {
	install := os.Getenv("UGAPP_INSTALL_DIR")
	if install == "" {
		// 平台没注入时（本地开发）从可执行文件位置反推：<install>/bin/deenmusic
		if exe, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				exe = resolved
			}
			install = filepath.Dir(filepath.Dir(exe))
		}
	}
	install, _ = filepath.Abs(install)

	data := envOr("UGAPP_DATA_DIR", filepath.Join(install, "data"))
	cache := envOr("UGAPP_CACHE_DIR", filepath.Join(install, "cache"))
	logDir := envOr("UGAPP_LOG_DIR", filepath.Join(install, "log"))

	service := filepath.Join(install, "service")
	return Paths{
		Install: install,
		Data:    data,
		Cache:   cache,
		Log:     logDir,

		Python:  filepath.Join(install, "runtime", "python", "bin", "python3.11"),
		PyHome:  filepath.Join(install, "runtime", "python"),
		Vendor:  filepath.Join(service, "vendor"),
		Service: service,
		Entry:   filepath.Join(service, "webapp", "ugos_main.py"),
		APIDir:  filepath.Join(service, "api"),

		Tmp: filepath.Join(cache, "tmp"),
	}
}

func (p Paths) ensureDirs() error {
	for _, dir := range []string{p.Data, p.Cache, p.Log, p.Tmp, filepath.Join(p.Data, "logs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// dirWritable 真的去写一个探针文件，而不是看权限位。
//
// 只 Stat 是不够的：授权目录在首次启动时压根还没 bind 进沙箱，那时它要么
// 不存在、要么是个不可写的合成视图，两种情况权限位都骗人。
func dirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	probe := filepath.Join(dir, ".deenmusic-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
}
