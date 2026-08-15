package main

import (
	"fmt"
	"strconv"
	"strings"
)

// 安装参数。平台把用户填的值写进 <安装目录>/init.d/<appid>.env，
// systemd 用 EnvironmentFile 加载 —— 到这里就是普通环境变量。
//
// ⚠ 装完的**第一次启动**这些值全是空的（平台先起服务、2~3 秒后才写配置），
// 所以每一项都必须有能独立工作的默认值，绝不能因为拿不到值就退出。
//
// ⚠ 这里【没有】WebUI 密码参数。密码统一由 qBittorrent 自己的界面管：
// 安装参数改密码要等到下一次启动才生效，而用户在 WebUI 里随手改过之后，
// 这个参数还会在某次重启时把它覆盖回去 —— 两头都不对，不如不给。
// 首次初始化写一个已知的默认密码（见 profile.go），之后一律在 WebUI 里改。
const (
	envDownloadPath  = "QB_DOWNLOAD_PATH"
	envBTPort        = "QB_BT_PORT"
	envTrackerUpdate = "QB_TRACKER_UPDATE"
	envTrackerURL    = "QB_TRACKER_URL"
)

// 镜像里 40-config 用的地址，作为默认值原样沿用。
const defaultTrackerURL = "https://githubraw.sleele.workers.dev/XIU2/TrackersListCollection/master/best.txt"

// BT 监听端口默认值。**不照抄上游的 6881**：用户很可能已经在同一台 NAS 上
// 跑着 Docker 版 qBittorrent，那边占的就是 6881。
const defaultBTPort = 26881

type params struct {
	DownloadPath  string // 空 = 用户没选，回落到应用数据目录
	BTPort        int
	TrackerUpdate bool
	TrackerURL    string

	// notes 收集"值不合法、已按默认处理"这类说明，启动时打进日志。
	// ⚠ 绝不因为某一项不合法就整体失败：多个独立的输入不该共用一次成败
	//（否则用户填错一个数字，填对的那几个也会跟着被丢掉）。
	notes []string
}

type lookupFunc func(string) (string, bool)

func loadParams(lookup lookupFunc) params {
	p := params{
		BTPort:        defaultBTPort,
		TrackerUpdate: true,
		TrackerURL:    defaultTrackerURL,
	}

	get := func(key string) string {
		v, ok := lookup(key)
		if !ok {
			return ""
		}
		// path 型参数一个都没选时平台写的是字面量 null（空列表 marshal 的结果）
		v = strings.TrimSpace(v)
		if v == "null" {
			return ""
		}
		return v
	}

	p.DownloadPath = get(envDownloadPath)
	if p.DownloadPath != "" && !strings.HasPrefix(p.DownloadPath, "/") {
		p.notes = append(p.notes, fmt.Sprintf("%s=%q 不是绝对路径，已忽略", envDownloadPath, p.DownloadPath))
		p.DownloadPath = ""
	}

	// ⚠ type: number 留空时平台可能写成 0，判空要把 0 也算上，
	// 否则"用户没填"会被当成"用户填了 0"撞上下限校验。
	if raw := get(envBTPort); raw != "" && raw != "0" {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			p.notes = append(p.notes, fmt.Sprintf("%s=%q 不是数字，已用默认端口 %d", envBTPort, raw, defaultBTPort))
		case n < 1024 || n > 65535:
			// 沙箱是非 root，<1024 根本 bind 不上。
			p.notes = append(p.notes, fmt.Sprintf("%s=%d 超出 1024-65535，已用默认端口 %d", envBTPort, n, defaultBTPort))
		default:
			p.BTPort = n
		}
	}

	if raw := get(envTrackerUpdate); raw != "" {
		switch strings.ToLower(raw) {
		case "true", "1", "yes", "on", "是":
			p.TrackerUpdate = true
		case "false", "0", "no", "off", "否":
			p.TrackerUpdate = false
		default:
			p.notes = append(p.notes, fmt.Sprintf("%s=%q 无法识别（可填 true/false），已按 true 处理", envTrackerUpdate, raw))
		}
	}

	if raw := get(envTrackerURL); raw != "" {
		if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
			p.notes = append(p.notes, fmt.Sprintf("%s=%q 不是 http(s) 地址，已用内置地址", envTrackerURL, raw))
		} else {
			p.TrackerURL = raw
		}
	}

	return p
}
