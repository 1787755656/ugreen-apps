package main

// 目录与可执行文件的定位。
//
// 沙箱里的目录布局（unit 文件里能看到是怎么围出来的）：
//
//	/var/packages/<appid>/            只读，装包解出来的东西都在这
//	  ├── bin/clouddl                 本管理壳
//	  ├── runtime/python/             自带的 CPython 3.11（python-build-standalone）
//	  ├── service/src/                上游 Python 源码 + ugos_main.py 适配层
//	  ├── service/vendor/             上游预装的第三方依赖
//	  └── www/                        前端静态页面（生产环境由网关 serve，这里是兜底）
//	/var/packages/<appid>/data|cache|log/   这三个才可写
//
// 【注意安装目录是只读的】——上游那套"配置写在应用根目录"的假设在这里不成立，
// 所有可写路径都必须落到 data/cache/log 里去。

import (
	"os"
	"path/filepath"
)

type Paths struct {
	Install string // 安装目录（只读）
	Data    string // 可写数据目录
	Cache   string // 可写缓存目录
	Log     string // 可写日志目录

	Python string // 解释器绝对路径
	PyHome string // PYTHONHOME
	Vendor string // PYTHONPATH（上游预装依赖）
	Src    string // 上游 Python 源码目录
	Entry  string // 入口脚本（ugos_main.py）
	WWW    string // 前端静态目录

	Config          string // 交给上游的 CONFIG_DIR
	Tmp             string // 交给上游的 TMPDIR —— 沙箱里【没有 /tmp】
	DownloadDirFile string // 上游用来记住"当前下载目录"的文件
	LauncherConfig  string // 管理壳自己的配置
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
		// 平台没注入时（本地开发）从可执行文件位置反推：<install>/bin/clouddl
		if exe, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				exe = resolved
			}
			install = filepath.Dir(filepath.Dir(exe))
		}
	}

	data := envOr("UGAPP_DATA_DIR", filepath.Join(install, "data"))
	cache := envOr("UGAPP_CACHE_DIR", filepath.Join(install, "cache"))
	log := envOr("UGAPP_LOG_DIR", filepath.Join(install, "log"))

	return Paths{
		Install: install,
		Data:    data,
		Cache:   cache,
		Log:     log,

		Python: filepath.Join(install, "runtime", "python", "bin", "python3.11"),
		PyHome: filepath.Join(install, "runtime", "python"),
		Vendor: filepath.Join(install, "service", "vendor"),
		Src:    filepath.Join(install, "service", "src"),
		Entry:  filepath.Join(install, "service", "src", "ugos_main.py"),
		WWW:    filepath.Join(install, "www"),

		Config:          filepath.Join(data, "config"),
		Tmp:             filepath.Join(cache, "tmp"),
		DownloadDirFile: filepath.Join(data, "download_dir"),
		LauncherConfig:  filepath.Join(data, "launcher.json"),
	}
}

// ensureDirs 建出所有必须存在的可写目录。
func (p Paths) ensureDirs() error {
	for _, dir := range []string{p.Data, p.Cache, p.Log, p.Config, p.Tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// dirWritable 真的去写一个探针文件，而不是看权限位。
//
// 只 Stat 是不够的：授权目录在首次启动时压根还没 bind 进沙箱，
// 那时它要么不存在、要么是个不可写的合成视图，两种情况权限位都骗人。
func dirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	probe := filepath.Join(dir, ".clouddl-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
}
