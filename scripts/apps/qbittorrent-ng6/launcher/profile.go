package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// 从 superng6/qbittorrent 镜像里原样取出的默认资源：
//   - qBittorrent.conf   针对国内网络调过的默认配置（含一大串内置 tracker）
//   - Search/*.py        33 个搜索引擎插件
//
// ⚠ 搜索插件要 Python 才能跑，而原生沙箱里没有 Python（`/usr/bin` 都不存在），
// 所以搜索功能在这个应用里用不了。插件照旧装进去只是保持和上游一致，
// 用户哪天自己想办法接上解释器时不用再补文件。
//
//go:embed assets/qBittorrent.conf
var defaultConf []byte

//go:embed assets/Search
var searchEngines embed.FS

// 首次初始化时给的 WebUI 密码。
//
// 为什么必须给一个已知密码：qBittorrent 5.x 在没设密码时会每次启动**随机生成**
// 一个临时密码、只打印在日志里。Docker 用户 `docker logs` 就能看到，
// 而绿联原生应用的日志普通用户根本够不着（没有 SSH），等于进不去 WebUI。
const initialWebUIPassword = "adminadmin"

type layout struct {
	dataDir    string
	cacheDir   string
	profileDir string
	confPath   string
	enginesDir string
}

func newLayout(dataDir, cacheDir string) layout {
	profile := filepath.Join(dataDir, "profile")
	return layout{
		dataDir:    dataDir,
		cacheDir:   cacheDir,
		profileDir: profile,
		confPath:   filepath.Join(profile, "qBittorrent", "config", "qBittorrent.conf"),
		enginesDir: filepath.Join(profile, "qBittorrent", "data", "nova3", "engines"),
	}
}

func (l layout) fallbackDownloadDir() string { return filepath.Join(l.dataDir, "downloads") }

// ensureProfile 建目录、铺默认配置和搜索插件。做成幂等的：每次启动都跑，
// 缺什么补什么 —— 上一次初始化只成功了一半时，重启一次就能自愈。
func ensureProfile(l layout) error {
	for _, dir := range []string{filepath.Dir(l.confPath), l.enginesDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("建目录 %s 失败: %w", dir, err)
		}
	}
	if _, err := os.Stat(l.confPath); os.IsNotExist(err) {
		if err := writeFileAtomic(l.confPath, defaultConf, 0o644); err != nil {
			return fmt.Errorf("写默认配置失败: %w", err)
		}
		logf("已写入默认配置 %s", l.confPath)
	} else if err != nil {
		return fmt.Errorf("读配置失败: %w", err)
	}
	return copySearchEngines(l.enginesDir)
}

func copySearchEngines(dst string) error {
	entries, err := fs.ReadDir(searchEngines, "assets/Search")
	if err != nil {
		return err
	}
	copied := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := searchEngines.ReadFile("assets/Search/" + e.Name())
		if err != nil {
			return err
		}
		target := filepath.Join(dst, e.Name())
		// 用户可能自己改过插件，已存在且内容一致就不动它。
		if old, err := os.ReadFile(target); err == nil && string(old) == string(data) {
			continue
		} else if err == nil {
			continue // 内容不同 = 用户或 qBittorrent 自己更新过，不覆盖
		}
		if err := writeFileAtomic(target, data, 0o644); err != nil {
			return fmt.Errorf("写搜索插件 %s 失败: %w", e.Name(), err)
		}
		copied++
	}
	if copied > 0 {
		logf("已安装 %d 个搜索引擎插件（注意：搜索功能需要 Python，沙箱里没有，实际用不了）", copied)
	}
	return nil
}

// applyParams 把安装参数落进 qBittorrent.conf，返回更新后的 state。
//
// ⚠ 每一项独立处理，任何一项失败都不影响其它项、更不会让应用起不来。
func applyParams(l layout, p params, st state) (state, error) {
	raw, err := os.ReadFile(l.confPath)
	if err != nil {
		return st, fmt.Errorf("读 %s 失败: %w", l.confPath, err)
	}
	conf := parseINI(raw)
	changed := false

	// ---- 下载目录 ----------------------------------------------------------
	desired := p.DownloadPath
	if desired != "" {
		if err := ensureWritableDir(desired); err != nil {
			logf("警告：下载目录 %s 不可写（%v），本次不改动配置里的保存路径", desired, err)
			logf("      如果刚安装或刚改过设置，请在应用中心把本应用【停止再启动】一次 —— "+
				"平台是先起服务、几秒后才写入授权目录，首次启动拿不到是正常的")
			desired = ""
		}
	}
	if desired == "" && !st.Initialized {
		desired = l.fallbackDownloadDir()
		if err := ensureWritableDir(desired); err != nil {
			return st, fmt.Errorf("兜底下载目录 %s 建不出来: %w", desired, err)
		}
		logf("尚未选择下载目录，暂时用 %s；请在应用设置里选一个媒体库目录后重启应用", desired)
	}
	if desired != "" && desired != st.AppliedDownloadPath {
		conf.SetPref(keySavePath, withTrailingSlash(desired))
		conf.SetPref(keyTempPath, withTrailingSlash(filepath.Join(desired, "incomplete")))
		st.AppliedDownloadPath = desired
		changed = true
		logf("下载保存目录 -> %s", desired)
	}

	// ---- BT 监听端口 -------------------------------------------------------
	if p.BTPort != st.AppliedBTPort {
		conf.SetPref(keyBTPort, strconv.Itoa(p.BTPort))
		st.AppliedBTPort = p.BTPort
		changed = true
		logf("BT 监听端口 -> %d（想提速的话记得在路由器上把这个端口转发到 NAS）", p.BTPort)
	}

	// ---- WebUI 密码 --------------------------------------------------------
	if ensureInitialPassword(conf) {
		changed = true
	}

	// ---- 首次初始化时收紧 WebUI 防护 ---------------------------------------
	// 上游镜像关掉了 CSRF / 点击劫持防护（方便套反向代理）。
	// 而原生应用是浏览器直连 NAS 的端口、局域网里谁都够得到，
	// 这两项该开着。用户之后想关照旧能在 WebUI 里关。
	if !st.Initialized {
		conf.Set("Preferences", `WebUI\CSRFProtection`, "true")
		conf.Set("Preferences", `WebUI\ClickjackingProtection`, "true")
		changed = true
	}

	if changed {
		if err := writeFileAtomic(l.confPath, conf.Bytes(), 0o644); err != nil {
			return st, fmt.Errorf("写配置失败: %w", err)
		}
	}
	return st, nil
}

// ensureInitialPassword 只管一件事：配置里必须有一个**已知的**密码。
//
// qBittorrent 5.x 在没设密码时会每次启动随机生成一个临时密码、只打印在日志里。
// Docker 用户 `docker logs` 看得到，原生应用的用户看不到 —— 等于进不去 WebUI。
//
// 已经有密码就绝不改动：那是用户在 WebUI 里自己设的。
func ensureInitialPassword(conf *iniFile) bool {
	if v, ok := conf.Get("Preferences", `WebUI\Password_PBKDF2`); ok && v != "" {
		return false
	}
	value, err := qbPasswordValue(initialWebUIPassword)
	if err != nil {
		logf("警告：生成密码摘要失败（%v）；qBittorrent 会改用随机临时密码，"+
			"届时请在本日志里搜索「临时密码」", err)
		return false
	}
	conf.Set("Preferences", `WebUI\Password_PBKDF2`, value)
	logf("WebUI 密码已设为默认值 %q —— 登录后请立刻在「工具 → 选项 → Web UI」里改掉",
		initialWebUIPassword)
	return true
}

func withTrailingSlash(p string) string {
	if p == "" || p[len(p)-1] == '/' {
		return p
	}
	return p + "/"
}

// ensureWritableDir 真的建目录并试写一个文件。
// ⚠ 只 Stat 是不够的：沙箱里 /volume1 会列出全部共享文件夹名，
// 但没授权的那些一 open 就是 No such file or directory —— 目录项可见 ≠ 能访问。
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".qb-write-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}
