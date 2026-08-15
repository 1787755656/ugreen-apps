package main

// 知音音乐（Zhiyin Music）—— UGOS Pro 原生应用的管理壳。
//
// 上游是 qwex333/zhiyin-music 这个 Docker 镜像（Rust + Axum + SQLite + TagLib，
// 自带完整前端、JWT 登录和一套 Subsonic API 兼容层）。本工程把它从容器里解出来
// 打成绿联原生应用，**上游二进制一个字节都没改**。
//
// 架构：
//
//	start_cmd → bin/zhiyin-launcher（本程序）
//	              ├─ 摆环境、建目录、按安装参数生成/合并 config.toml
//	              └─ syscall.Exec 换成 bin/zhiyin-music（pid 不变）
//
// 为什么是 exec 而不是 fork 出去守护：
//
//   - 上游自己处理 SIGTERM，systemd 直接把信号发给它最干净；pid 不变的话
//     unit 的 MainPID、日志重定向、TimeoutStopSec 全都对得上。
//   - 沙箱里 `/proc/self` 是【钉死在主进程 pid 上】的绑定，fork 出来的子进程
//     读它会读到父进程的（GraalVM 应用就是这么挂的）。exec 不换 pid，天然没这问题。
//   - 代价是没有自动重启和日志前缀。前者交给 systemd，后者本来也不需要 ——
//     上游的日志已经很完整，管理壳只在 exec 之前把最值钱的诊断打出来。
//
// 声明端口就是上游自己监听的端口（open_type: tab，浏览器直连），
// 本管理壳不占端口、也不做反向代理 —— 音频流走代理只会平白多一次拷贝。

import (
	"flag"
	"fmt"
	"log"
	"os"
	"syscall"
)

// defaultPort 必须和 project.yaml 的 port、start_cmd 里的 --port 一致，
// scripts/build.sh 里有断言钉着这三处。
const defaultPort = 28085

func main() {
	port := flag.Int("port", defaultPort, "HTTP 端口，必须和 project.yaml 的 port 一致")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime)
	log.SetPrefix("[zhiyin] ")

	if err := run(*port); err != nil {
		log.Fatalf("%v", err)
	}
}

func run(port int) error {
	paths, err := resolvePaths()
	if err != nil {
		return err
	}
	if err := paths.checkPayload(); err != nil {
		return err
	}
	if err := paths.ensureDirs(); err != nil {
		return err
	}

	params := readParams(os.Getenv)

	log.Printf("知音音乐 —— 安装目录 %s", paths.Root)
	log.Printf("数据目录 %s", paths.Data)
	log.Printf("缓存目录 %s", paths.Cache)
	logMusicPaths(params)

	// 配置文件是【相对 cwd】解析的（真机之前在容器里做过对照实验：
	// cwd=/elsewhere 就读 /elsewhere/config.toml，cwd=/ 读不到就全用默认值）。
	// 所以下面 exec 之前必须 chdir 到数据目录，config.toml 也写在那儿 ——
	// 用户在应用里改配置（PUT /api/config）时上游写回的也是这个文件。
	added, removed, err := syncConfig(paths, port, params)
	if err != nil {
		return fmt.Errorf("准备配置文件失败：%w", err)
	}
	logRootChanges(added, removed)

	// 更新日志那一页的数据。失败不致命 —— 只是应用里的"更新日志"空着。
	if err := paths.syncReleases(); err != nil {
		log.Printf("更新日志没能放到位（应用内那一页会是空的，不影响播放）：%v", err)
	}

	// 「扫描不到音乐」是这类应用最常见的报障，而用户没有 SSH、上游日志也只会说
	// "找到 0 个音频文件"。这里把每个扫描目录的实际情况翻译成人话写进日志。
	for _, line := range diagnoseRoots(configuredRoots(paths)) {
		log.Print(line)
	}

	// 端口被占的话上游只会留一行很难懂的报错就退出（应用中心显示"未启动"），
	// 所以在 exec 之前自己先探一次，把话说清楚。
	if err := checkPortFree(port); err != nil {
		log.Printf("⚠ %v", err)
	}

	logAdminReminder()

	if err := os.Chdir(paths.Data); err != nil {
		return fmt.Errorf("切到数据目录 %s 失败：%w", paths.Data, err)
	}

	env := childEnv(paths)
	log.Printf("在端口 %d 上启动服务（下面开始是上游自己的日志）", port)

	// syscall.Exec 成功就不会返回 —— 本进程整个被上游替换掉。
	if err := syscall.Exec(paths.Server, []string{paths.Server}, env); err != nil {
		return fmt.Errorf("启动 %s 失败：%w", paths.Server, err)
	}
	return nil
}

func logMusicPaths(p Params) {
	if len(p.MusicPaths) == 0 {
		// 首启必然走这条，不是错误 —— 平台是先起服务、2~3 秒后才写 .env 和
		// unit 的，参数值和授权目录第一次启动【都】是空的。
		log.Printf("没有拿到音乐目录。如果刚安装或升级过，请在应用中心「停止 → 启动」一次；" +
			"也可以先进应用，在「设置」里手动填写目录。")
		return
	}
	log.Printf("音乐目录（共 %d 个，已授权给本应用读写）：", len(p.MusicPaths))
	for _, d := range p.MusicPaths {
		if _, err := os.Stat(d); err != nil {
			log.Printf("  %s  ← 沙箱里读不到，多半是首次启动授权还没生效，重启一次即可", d)
			continue
		}
		log.Printf("  %s", d)
	}
}

func logRootChanges(added, removed []string) {
	for _, d := range added {
		log.Printf("已把 %s 加进扫描目录", d)
	}
	for _, d := range removed {
		log.Printf("已把 %s 移出扫描目录（安装参数里去掉了它，沙箱里也不再有权限）", d)
	}
}

func logAdminReminder() {
	// tab 型应用的端口在局域网上直接可达，这条要显眼。
	// 账号是在应用自己的引导页里创建的（上游首次打开就会提示），我们不插手 ——
	// 但得提醒用户【立刻】去创建，否则同网段的人能抢先把账号占掉。
	log.Printf("⚠ 如果还没有创建过管理员账号，请【立刻】用浏览器打开本应用，" +
		"在首次进入的引导页里创建 —— 本应用的端口在局域网上是直接可达的" +
		"（Subsonic 客户端正是靠这个直连），在你创建之前，同网段的任何人都能抢先创建并拿到你的音乐库。")
}
