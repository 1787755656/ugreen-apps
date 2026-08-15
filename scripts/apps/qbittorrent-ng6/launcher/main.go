// qBittorrent 绿联原生应用的管理壳。
//
// 它替代 superng6/qbittorrent 镜像里那套 s6 脚本（cont-init.d/*、services.d/qbittorrent/run），
// 把同样的事在原生沙箱里做一遍：铺默认配置、装搜索插件、更新 tracker 列表、
// 设好环境变量、拉起 qbittorrent-nox 并守护它。
//
// 沙箱里必须自己摆平的几件事（每一条都有对应的血泪来源）：
//   - 没有 /tmp     → TMPDIR 指到应用缓存目录，否则 Qt 建临时文件直接失败
//   - 没有 /etc/passwd → getpwuid 必然失败，HOME 必须显式给
//   - 没有 /etc/hosts  → localhost 解析不了，一律写 127.0.0.1
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func logf(format string, args ...any) {
	fmt.Printf("%s [launcher] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

func main() {
	port := flag.Int("port", 0, "WebUI 监听端口（与 project.yaml 的 port 保持一致）")
	flag.Parse()
	if *port <= 0 {
		logf("错误：必须用 --port 指定 WebUI 端口")
		os.Exit(2)
	}

	dataDir := envOr("UGAPP_DATA_DIR", "")
	if dataDir == "" {
		logf("错误：拿不到 UGAPP_DATA_DIR，这个程序只能由绿联平台拉起")
		os.Exit(2)
	}
	cacheDir := envOr("UGAPP_CACHE_DIR", filepath.Join(dataDir, "cache"))

	l := newLayout(dataDir, cacheDir)
	// 沙箱里没有 /tmp，Qt / qBittorrent 建临时文件全指望 TMPDIR。
	tmpDir := filepath.Join(cacheDir, "tmp")
	for _, dir := range []string{cacheDir, tmpDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logf("错误：建目录 %s 失败：%v", dir, err)
			os.Exit(1)
		}
	}

	p := loadParams(os.LookupEnv)
	for _, note := range p.notes {
		logf("参数提示：%s", note)
	}

	st := loadState(dataDir)
	if !st.Initialized {
		logf("首次初始化（数据目录 %s）", dataDir)
	}

	if err := ensureProfile(l); err != nil {
		logf("错误：初始化配置目录失败：%v", err)
		os.Exit(1)
	}

	// 自带的 IP 地理数据库，首次启动直接铺过去，省掉那条
	//「无法加载 IP 地理数据库」的告警（以及对 db-ip.com 的联网依赖）。
	ensureGeoIPDatabase(l, installDir())

	st, err := applyParams(l, p, st)
	if err != nil {
		// 配置改不动就按现有配置继续跑，别让应用起不来。
		logf("警告：应用安装参数失败：%v（将按现有配置启动）", err)
	}

	if p.TrackerUpdate {
		updateTrackers(l, p)
	} else {
		logf("未开启 tracker 列表自动更新")
	}

	// 初始化完成标记只在**全部成功后**才落盘。
	st.Initialized = true
	if err := saveState(dataDir, st); err != nil {
		logf("警告：保存状态失败：%v（下次启动会重新应用一遍参数）", err)
	}

	// 下载下来的文件要让 NAS 用户在文件管理器里删得掉。
	// 原生应用跑在平台建的独立用户下，进不了 admin 组，只能靠放开 other 位。
	syscall.Umask(0)

	bin := filepath.Join(filepath.Dir(mustExecutable()), "qbittorrent-nox")
	if _, err := os.Stat(bin); err != nil {
		logf("错误：找不到 qbittorrent-nox（%v）", err)
		os.Exit(1)
	}

	logf("WebUI 端口 %d，配置目录 %s", *port, l.profileDir)
	logf("打开方式：浏览器访问 http://<NAS 的局域网 IP>:%d ，默认账号 admin", *port)

	sup := &supervisor{
		bin: bin,
		args: []string{
			"--profile=" + l.profileDir,
			"--webui-port=" + strconv.Itoa(*port),
			"--confirm-legal-notice",
		},
		env: childEnv(l, tmpDir),
	}
	os.Exit(sup.run())
}

// childEnv 只给子进程它真正需要的那几个变量，别把管理壳的环境整个倒过去
//（里面还有安装参数，其中包含密码）。
func childEnv(l layout, tmpDir string) []string {
	env := []string{
		"HOME=" + l.profileDir,
		"TMPDIR=" + tmpDir,
		"XDG_CONFIG_HOME=" + filepath.Join(l.profileDir, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(l.profileDir, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(l.cacheDir, "xdg"),
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	if tz, ok := os.LookupEnv("TZ"); ok {
		env = append(env, "TZ="+tz)
	}
	return env
}

// tracker 列表要在 qBittorrent 启动【之前】写进配置（跑起来之后改会被它整体覆盖），
// 所以这段网络请求是卡在启动路径上的。超时给得短一点：平台靠探测端口判断
// "启动成功没有"，网络不通时干等半分钟会被判成启动失败。
const trackerFetchTimeout = 12 * time.Second

func updateTrackers(l layout, p params) {
	ctx, cancel := context.WithTimeout(context.Background(), trackerFetchTimeout)
	defer cancel()

	client := &http.Client{Timeout: trackerFetchTimeout}
	list, err := fetchTrackerList(ctx, client, p.TrackerURL)
	if err != nil {
		// 拿不到就用配置里现有的那份，绝不因此耽误启动。
		logf("更新 tracker 列表失败：%v（沿用现有列表继续启动）", err)
		return
	}

	raw, err := os.ReadFile(l.confPath)
	if err != nil {
		logf("更新 tracker 列表失败：读配置 %v", err)
		return
	}
	conf := parseINI(raw)
	if old, ok := conf.GetPref(keyTrackers); ok && old == list {
		logf("tracker 列表已是最新，无需更新")
		return
	}
	conf.SetPref(keyTrackers, list)
	if err := writeFileAtomic(l.confPath, conf.Bytes(), 0o644); err != nil {
		logf("更新 tracker 列表失败：写配置 %v", err)
		return
	}
	logf("已更新 tracker 列表（%d 个）", strings.Count(list, `\n`)+1)
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// installDir 是安装目录，也就是 bin/ 的上一级 —— rootfs_common 里的东西
//（icon.png、www/、geoip/）都铺在这里。
func installDir() string {
	return filepath.Dir(filepath.Dir(mustExecutable()))
}

// mustExecutable：管理壳自己就是 systemd 起的主进程，
// 沙箱里 /proc/self 钉死在主进程上，所以这里读到的就是自己，可靠。
func mustExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		logf("警告：定位自身路径失败（%v），按当前目录找 qbittorrent-nox", err)
		return "./launcher"
	}
	return exe
}
