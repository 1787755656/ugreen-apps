package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Paths 汇集沙箱内所有会用到的工作目录。
// UGOS 原生应用只可写 data/log/cache/shared，安装目录只读，
// 因此除 www（静态资源）与 bin（引擎/ffmpeg）外一律落在 data 目录下。
type Paths struct {
	Install string // 安装目录（只读）
	Data    string
	Log     string
	Cache   string
	Shared  string
	WWW     string
	Tasks   string
	Tmp     string

	EngineBundled string // 包内自带引擎
	EngineData    string // 应用内更新后的引擎（优先使用）
	FFmpeg        string // 可选的 ffmpeg（混流用）
	Download      string // 默认下载目录
}

// envDir 读取环境变量形式的目录；平台在未选择 path 参数时会写字面量 "null"。
func envDir(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" || v == "null" {
		return def
	}
	return filepath.Clean(v)
}

func firstWritable(cands ...string) string {
	for _, d := range cands {
		if d == "" || d == "null" {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err == nil {
			if w, err := os.CreateTemp(d, ".wtest"); err == nil {
				name := w.Name()
				w.Close()
				os.Remove(name)
				return d
			}
		}
	}
	return ""
}

func resolvePaths() (*Paths, error) {
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	install := envDir("UGAPP_INSTALL_DIR", filepath.Dir(filepath.Dir(exe)))

	p := &Paths{
		Install: install,
		Data:    envDir("UGAPP_DATA_DIR", filepath.Join(install, "data")),
		Log:     envDir("UGAPP_LOG_DIR", filepath.Join(install, "log")),
		Cache:   envDir("UGAPP_CACHE_DIR", filepath.Join(install, "cache")),
		Shared:  envDir("UGAPP_SHARED_DIR", filepath.Join(install, "shared")),
		WWW:     filepath.Join(install, "www"),
	}
	for _, d := range []string{p.Data, p.Log, p.Cache, p.Shared} {
		_ = os.MkdirAll(d, 0o755)
	}
	p.Tasks = filepath.Join(p.Data, "tasks")
	p.Tmp = filepath.Join(p.Data, "tmp")
	_ = os.MkdirAll(p.Tasks, 0o755)
	_ = os.MkdirAll(p.Tmp, 0o755)

	// .NET 单文件自解压需要可写 TMPDIR；沙箱没有 /tmp，必须自己指定。
	os.Setenv("TMPDIR", p.Tmp)
	os.Setenv("TMP", p.Tmp)
	os.Setenv("TEMP", p.Tmp)
	os.Setenv("HOME", p.Data)
	os.Setenv("DOTNET_BUNDLE_EXTRACT_BASE_DIR", filepath.Join(p.Cache, "dotnet"))
	_ = os.MkdirAll(filepath.Join(p.Cache, "dotnet"), 0o755)

	p.EngineBundled = filepath.Join(install, "bin", "N_m3u8DL-RE")
	p.EngineData = filepath.Join(p.Data, "engine", "N_m3u8DL-RE")
	if _, err := os.Stat(p.EngineData); err != nil {
		p.EngineData = ""
	}

	ff := filepath.Join(p.Data, "ffmpeg", "ffmpeg")
	if _, err := os.Stat(ff); err != nil {
		ff = filepath.Join(install, "bin", "ffmpeg")
		if _, err := os.Stat(ff); err != nil {
			ff = ""
		}
	}
	p.FFmpeg = ff

	// 下载目录优先级：应用设置里选的 path 参数 → 应用共享目录 → 数据目录。
	// 参数未选时可能是 "null"，也可能是空串；两种情况都回退。
	p.Download = firstWritable(
		strings.TrimSpace(os.Getenv("DOWNLOAD_DIR")),
		filepath.Join(p.Shared, "downloads"),
		filepath.Join(p.Data, "downloads"),
	)
	if p.Download == "" {
		return nil, fmt.Errorf("没有可写的下载目录（已尝试 DOWNLOAD_DIR / shared/downloads / data/downloads）")
	}
	return p, nil
}

// Engine 返回当前生效的引擎路径：应用内更新过的优先，否则用包内自带的。
func (p *Paths) Engine() string {
	if p.EngineData != "" {
		if _, err := os.Stat(p.EngineData); err == nil {
			return p.EngineData
		}
	}
	return p.EngineBundled
}

// allowedRoots 是文件下载/删除/目录浏览允许触及的根集合。
// 只包含沙箱里真正授权的目录，避免越权访问其它卷。
func (p *Paths) allowedRoots(extra ...string) []string {
	roots := []string{p.Download, p.Shared}
	roots = append(roots, extra...)
	out := roots[:0]
	seen := map[string]bool{}
	for _, r := range roots {
		r = filepath.Clean(strings.TrimSpace(r))
		if r == "" || r == "." || r == "null" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// underAny 判断 abs 是否落在某个根目录内（含根本身）。
func underAny(abs string, roots []string) bool {
	abs = filepath.Clean(abs)
	for _, r := range roots {
		if abs == r || strings.HasPrefix(abs, r+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// realPath 解析软链，返回真实路径（UGOS 文件管理器需要 /volume1/... 真实路径）。
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
