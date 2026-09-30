package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Paths 是所有目录的唯一真相来源。平台注入的 UGAPP_* 环境变量在这里统一解析，
// 其它地方一律不再直接读环境变量。
type Paths struct {
	Install string // 安装目录，只读
	Data    string // 应用数据目录（平台给的，可写）
	Log     string // 日志目录
	Cache   string // 缓存目录，同时用作 TMPDIR

	Base     string // <Install>/mariadb —— 就是 mariadbd 的 basedir
	Loader   string // <Base>/lib/ld-linux-<arch>.so.N
	LibDir   string // <Base>/lib
	Mariadbd string
	Client   string // mariadb 客户端
	Upgrade  string // mariadb-upgrade
	Dump     string // mariadb-dump
	ShareDir string // <Base>/share
}

// loaderName 按【当前进程的架构】选动态加载器的文件名。
// 管理壳自己是交叉编译出来的 Go 静态二进制，跑在哪个架构上，
// 同一个包里的 mariadbd 就是哪个架构，所以用 runtime.GOARCH 判断是准的。
func loaderName() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "ld-linux-x86-64.so.2", nil
	case "arm64":
		return "ld-linux-aarch64.so.1", nil
	default:
		return "", fmt.Errorf("不支持的架构 %s（本应用只打包了 amd64/arm64）", runtime.GOARCH)
	}
}

func envDir(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func ResolvePaths() (*Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("定位自身可执行文件失败: %w", err)
	}
	// 默认安装目录 = 可执行文件所在目录的上一级（bin/ 的父目录）
	defaultInstall := filepath.Dir(filepath.Dir(exe))

	p := &Paths{
		Install: envDir("UGAPP_INSTALL_DIR", defaultInstall),
	}
	p.Data = envDir("UGAPP_DATA_DIR", filepath.Join(p.Install, "data"))
	p.Log = envDir("UGAPP_LOG_DIR", filepath.Join(p.Install, "log"))
	p.Cache = envDir("UGAPP_CACHE_DIR", filepath.Join(p.Data, "cache"))

	p.Base = filepath.Join(p.Install, "mariadb")
	p.LibDir = filepath.Join(p.Base, "lib")
	p.ShareDir = filepath.Join(p.Base, "share")
	p.Mariadbd = filepath.Join(p.Base, "sbin", "mariadbd")
	p.Client = filepath.Join(p.Base, "bin", "mariadb")
	p.Upgrade = filepath.Join(p.Base, "bin", "mariadb-upgrade")
	p.Dump = filepath.Join(p.Base, "bin", "mariadb-dump")

	ld, err := loaderName()
	if err != nil {
		return nil, err
	}
	p.Loader = filepath.Join(p.LibDir, ld)

	for _, d := range []string{p.Data, p.Log, p.Cache} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("创建目录 %s 失败: %w", d, err)
		}
	}
	return p, nil
}

// Check 在启动最早期验证包的完整性。缺文件的表现如果放到运行时才暴露，
// 就是一行 "no such file or directory"，从日志上根本看不出缺的是什么。
func (p *Paths) Check() error {
	for _, f := range []string{p.Mariadbd, p.Loader, p.Client, p.Upgrade} {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("包不完整，缺少 %s: %w", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(p.ShareDir, "english", "errmsg.sys")); err != nil {
		return fmt.Errorf("包不完整，缺少 share/english/errmsg.sys: %w", err)
	}
	return nil
}
