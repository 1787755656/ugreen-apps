package main

// 管理壳自己的配置，以及"下载目录到底用哪个"这件事。
//
// 三个地方都可能说自己是下载目录，优先级必须钉死，否则用户会遇到
// "重启一次文件就换地方了"：
//
//	① 安装参数 CLOUDDL_DOWNLOAD_DIR   用户在应用中心的设置弹窗里选的
//	② <data>/download_dir             上游自己维护的"当前目录"（用户在应用内改的）
//	③ <data>/downloads                兜底
//
// 规则：**启动时只认 ②**，绝不每次都跟着 ① 漂移 —— 因为上游允许用户在
// 「设置 - 下载策略」里改目录，跟着参数漂会把用户在应用里的选择每次开机冲掉。
// 只有在【参数值相对上次启动发生了变化】时才采纳 ①（那说明用户刚在应用中心改过），
// 采纳完把新值写进 ② 并记下来。首次启动 ② 不存在，直接采纳 ①。
//
// 还有一条来自平台的坑：全新安装/升级后的**第一次启动，参数值和授权目录都是空的**
// （平台先起服务、2~3 秒后才写 .env 和 unit）。所以读到空值是【正常】的，
// 不能报错、更不能把已有配置清掉，走兜底目录把应用先跑起来即可。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type launcherConfig struct {
	// LastParamDir 是"上一次启动时安装参数的值"，用来识别用户改没改过参数。
	LastParamDir string `json:"last_param_dir"`
}

func loadLauncherConfig(path string) launcherConfig {
	var cfg launcherConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func saveLauncherConfig(path string, cfg launcherConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// paramDownloadDir 读安装参数。
//
// multi: false 的 path 参数在 .env 里是【裸标量】（不是 Docker 型那种 JSON 列表）。
// 但仍要把 "null" 当空：平台在"一个都没选"时写的就是这个字面量。
func paramDownloadDir() string {
	v := strings.TrimSpace(os.Getenv("CLOUDDL_DOWNLOAD_DIR"))
	if v == "" || v == "null" {
		return ""
	}
	if !filepath.IsAbs(v) {
		return ""
	}
	return filepath.Clean(v)
}

// readDownloadDirFile 读上游维护的"当前下载目录"。
func readDownloadDirFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if line == "" || !filepath.IsAbs(line) {
		return ""
	}
	return filepath.Clean(line)
}

func writeDownloadDirFile(path, dir string) error {
	return os.WriteFile(path, []byte(dir+"\n"), 0o600)
}

// downloadDirDecision 是解析结果，同时带上给用户看的说明。
// Note 会出现在 /api/status 和日志里 —— 目录不对是这个应用最常见的求助，
// 别让用户只看到"下载失败"。
type downloadDirDecision struct {
	Dir      string
	Fallback bool   // 是否退到了应用私有目录
	Note     string // 人话说明，可能为空
}

// resolveDownloadDir 按上面的优先级定下这次启动用哪个目录。
func resolveDownloadDir(p Paths, cfg *launcherConfig) downloadDirDecision {
	param := paramDownloadDir()
	current := readDownloadDirFile(p.DownloadDirFile)

	var chosen string
	var note string

	switch {
	case current == "":
		// 首次启动：有参数就用参数
		chosen = param
		if chosen == "" {
			note = "安装时没有选下载目录（或这是安装后的第一次启动，参数还没写进来）。"
		}
	case param != "" && param != cfg.LastParamDir:
		// 用户在应用中心改过参数 —— 这一次跟着参数走
		chosen = param
		note = "检测到安装参数里的下载目录已改为 " + param + "，本次启动起改用它。"
	default:
		// 平时：只认应用内的选择，绝不跟参数漂
		chosen = current
	}

	if chosen != "" && !dirWritable(chosen) {
		note = "下载目录 " + chosen + " 现在写不进去。" +
			"如果刚安装或刚改过参数，请在应用中心「停止 → 启动」一次" +
			"（平台要重启后才会把授权目录挂进来）。这次先把文件放在应用私有目录里。"
		chosen = ""
	}

	if chosen == "" {
		fallback := filepath.Join(p.Data, "downloads")
		_ = os.MkdirAll(fallback, 0o755)
		return downloadDirDecision{Dir: fallback, Fallback: true, Note: note}
	}

	// 记下这次的参数值，下次才判断得出"用户改没改过"
	if param != cfg.LastParamDir {
		cfg.LastParamDir = param
		_ = saveLauncherConfig(p.LauncherConfig, *cfg)
	}
	// 把最终结果同步给上游（它自己也读这个文件）
	if chosen != current {
		_ = writeDownloadDirFile(p.DownloadDirFile, chosen)
	}
	return downloadDirDecision{Dir: chosen, Note: note}
}

// ensureDefaultSettings 只在【settings.json 还不存在】时写一份默认设置。
//
// 唯一需要改的默认值是 aria2_enabled：上游默认开着并连 127.0.0.1:6800，
// 而绿联 NAS 上那个端口很可能已经被别的下载应用（迅雷、qBittorrent 之流）
// 自带的 aria2 占着 —— 沙箱和宿主共用 loopback，连过去就是在指挥别人的 aria2
// 把文件下到别人的目录里。本包也【没有】自带 aria2，所以默认关掉。
// 用户自己在 NAS 上跑了 aria2 的，仍可在设置里打开并填 RPC 地址与密钥。
//
// 上游的 SettingsStore.load() 会把文件内容 merge 进默认值，所以只写这一个键就够。
func ensureDefaultSettings(configDir string) error {
	path := filepath.Join(configDir, "settings.json")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	payload := map[string]any{"aria2_enabled": false}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
