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

	// 下载目录的现场，给界面/诊断用。
	Download       string // 实际生效的下载目录
	DownloadSource string // env | unit | shared | data
	DownloadActive bool   // Download 是否真的可写
	DeclaredPath   string // 平台声明授权、但沙箱里还用不了的目录（pending）
	DeclaredErr    string

	Fallback string // 声明目录不可用时的兜底目录（data/downloads）
}

// envDir 读取环境变量形式的目录；平台在未选择 path 参数时会写字面量 "null"。
func envDir(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" || v == "null" {
		return def
	}
	return filepath.Clean(v)
}

// writableDir 判断目录存在且当前进程能真写（不只看权限位，直接落一个临时文件）。
func writableDir(d string) bool {
	if d == "" {
		return false
	}
	st, err := os.Stat(d)
	if err != nil || !st.IsDir() {
		return false
	}
	f, err := os.CreateTemp(d, ".wtest")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// ensureDir 只对「自己该创建的目录」动手（data/cache/log 这类），
// 用户授权目录绝不去 mkdir —— 那目录归平台和用户管。
func ensureDir(d string) string {
	if d == "" {
		return ""
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return ""
	}
	if !writableDir(d) {
		return ""
	}
	return d
}

// quickDataDir 在 resolvePaths 之前先算出 data 目录（自愈状态文件要放这儿）。
func quickDataDir() string {
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	install := envDir("UGAPP_INSTALL_DIR", filepath.Dir(filepath.Dir(exe)))
	return envDir("UGAPP_DATA_DIR", filepath.Join(install, "data"))
}

// unitFilePath 返回本应用的 systemd unit 路径（沙箱里 /lib 只读挂载，0644 可读）。
func unitFilePath(install string) string {
	appID := filepath.Base(strings.TrimRight(install, "/"))
	for _, dir := range []string{"/lib/systemd/system", "/usr/lib/systemd/system", "/etc/systemd/system"} {
		p := filepath.Join(dir, appID+".service")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// DeclaredDownloadDirs 从 unit 的 BindPaths= 里取「平台已经授权给用户的目录」。
//
// 为什么读 unit 而不是 init.d/*.env：
//   - init.d/ 是 0700 root，沙箱里的应用进程读不到（实测 from_file 永远为空）；
//   - unit 是 0644，**在挂载生效之前**就能读到平台"打算授权什么"。
//
// BindPaths 有 `BindPaths=-"/volume1/x"` 这种写法（`-` 表示挂载失败可忽略），
// 需要去掉前缀与引号；只保留 /volumeN 下、且不属于本应用自己的
// data/cache/log/shared（那些是 UGAPP_* 目录，不是用户授权目录）。
func DeclaredDownloadDirs(install string) []string {
	unit := unitFilePath(install)
	if unit == "" {
		return nil
	}
	raw, err := os.ReadFile(unit)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "BindPaths=") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "BindPaths="))
		v = strings.TrimPrefix(v, "-")
		v = strings.Trim(v, `"`)
		v = strings.Trim(v, "'")
		if !strings.HasPrefix(v, "/volume") || strings.Contains(v, "/@") {
			continue
		}
		if strings.HasPrefix(v, install+string(os.PathSeparator)) {
			continue
		}
		out = append(out, filepath.Clean(v))
	}
	return out
}

// resolvePaths 解析全部工作目录。
//
// 下载目录的优先级：
//  1. 环境变量 DOWNLOAD_DIR（path 参数，平台在服务启动后约 1.6 秒才写）
//  2. unit 里的 BindPaths=（平台的权威授权声明，启动后约 4 秒出现）
//  3. shared/downloads
//  4. data/downloads
//
// ⚠ 关键：1/2 存在但沙箱里用不了时（全新安装的第一次启动就是这个状态 ——
// 参数还没写、挂载还没建），绝不静默回退到 data 目录（用户根本看不到那里），
// 而是记成 pending，由 main 的自愈逻辑主动退出一次让 systemd 重启（那时参数与
// 挂载都已就位）。只有在自愈配额用尽后，才退到兜底目录并在界面挂横幅。
func resolvePaths(healExhausted bool) (*Paths, error) {
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
	for _, d := range []string{p.Data, p.Log, p.Cache} {
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

	p.Fallback = ensureDir(filepath.Join(p.Data, "downloads"))
	if p.Fallback == "" {
		p.Fallback = p.Data // data 一定可写，纯保险
	}

	// 1) 参数（环境变量）  2) unit 里的授权声明
	var declared, declaredSrc string
	if v := envDir("DOWNLOAD_DIR", ""); v != "" {
		declared, declaredSrc = v, "env"
	} else if dirs := DeclaredDownloadDirs(install); len(dirs) > 0 {
		declared, declaredSrc = dirs[0], "unit"
	}

	if declared != "" {
		if writableDir(declared) {
			p.Download = declared
			p.DownloadSource = declaredSrc
			p.DownloadActive = true
			return p, nil
		}
		p.DeclaredPath = declared
		p.DeclaredErr = "平台已声明该目录，但沙箱里不可写"
		if _, err := os.Stat(declared); err != nil {
			p.DeclaredErr = "平台已授权该目录，但沙箱里还不存在（挂载尚未生效）"
		}
		if healExhausted {
			// 自愈配额用尽：先保证能干活，界面挂横幅说清楚
			p.Download, p.DownloadSource, p.DownloadActive = p.Fallback, "data", true
		} else {
			p.Download, p.DownloadSource, p.DownloadActive = declared, declaredSrc, false
		}
		return p, nil
	}

	// 3) shared/downloads（没给授权目录时的兜底）
	if d := ensureDir(filepath.Join(p.Shared, "downloads")); d != "" {
		p.Download, p.DownloadSource, p.DownloadActive = d, "shared", true
		return p, nil
	}
	// 4) 最后的兜底：应用数据目录（绿联文件管理器看不到，界面要提示）
	p.Download, p.DownloadSource, p.DownloadActive = p.Fallback, "data", true
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
// 只包含沙箱里真正授权（或声明过）的目录，避免越权访问其它卷。
func (p *Paths) allowedRoots(extra ...string) []string {
	roots := []string{p.Download, p.Fallback, p.Shared}
	if p.DeclaredPath != "" {
		roots = append(roots, p.DeclaredPath)
	}
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

// downloadStatus 给界面用的下载目录现场（前端据此挂横幅）。
func (p *Paths) downloadStatus() map[string]any {
	m := map[string]any{
		"path":    p.Download,
		"source":  p.DownloadSource,
		"active":  p.DownloadActive,
		"warning": "",
	}
	switch {
	case !p.DownloadActive && p.DeclaredPath != "":
		m["warning"] = fmt.Sprintf("已授权目录 %s 还没生效，应用正在自动重启，请稍候几秒", p.DeclaredPath)
		m["declared"] = p.DeclaredPath
		m["declaredError"] = p.DeclaredErr
	case p.DownloadSource == "data":
		m["warning"] = "没有可用的授权目录，文件会存到应用数据目录（绿联文件管理器里看不到）。请到「应用中心 → m3u8下载器 → 设置」选择下载目录。"
	case p.DownloadSource == "shared":
		m["warning"] = "还没选择下载目录，文件存在应用共享目录；建议在应用设置里明确选一个文件夹。"
	}
	return m
}
