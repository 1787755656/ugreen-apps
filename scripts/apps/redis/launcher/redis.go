package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

func loaderName() string {
	if runtime.GOARCH == "amd64" {
		return "ld-linux-x86-64.so.2"
	}
	return "ld-linux-aarch64.so.1"
}

// redisCommand 构造一条运行自带 redis-server 的命令。
//
// 【用自带的 loader】沙箱里没有 /usr/lib，系统 loader 在不在、在哪都不由我们决定。
// 所以显式地跑 ld-linux --library-path <我们的runtime> <真身>.bin。
//
// name 是 bin/ 下的名字（redis-server / redis-check-rdb / redis-check-aof）。
// 后两个是指向 redis-server.bin 的软链 —— Redis 靠 argv[0] 分流，而用显式 loader
// 启动时 argv[0] 就是我们给的这个路径，所以分流照常生效。
func (a *App) redisCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	loader := filepath.Join(a.runtime, loaderName())
	real := a.binPath(name)
	full := append([]string{"--library-path", a.runtime, real}, args...)

	cmd := exec.CommandContext(ctx, loader, full...)
	cmd.Dir = a.DataBase()
	cmd.Env = []string{
		"LD_LIBRARY_PATH=" + a.runtime,
		"HOME=" + a.dataDir,
		// 沙箱里【没有 /tmp】，一律指到我们自己的目录。
		"TMPDIR=" + filepath.Join(a.dataDir, "tmp"),
		// 日志时间戳要跟着 NAS 的时区。沙箱的 /etc 里恰好有 localtime，
		// 所以不设 TZ 就能对 —— 这也是 Redis 不需要像 PostgreSQL 那样
		// 自带 tzdata 的原因。
	}
	return cmd
}

// binPath 给出要交给 loader 的可执行文件路径。
//
// 包里【只有 redis-server.bin 一个可执行文件】。redis-check-rdb / redis-check-aof
// 是同一个二进制按 argv[0] 分流出来的模式，所以别名走数据目录里的软链
// （见 ensureBinAliases —— 软链不能打进包，ugcli 会把它解引用成完整拷贝）。
func (a *App) binPath(name string) string {
	if name == "redis-server" {
		return filepath.Join(a.binDir, "redis-server.bin")
	}
	return filepath.Join(a.aliasDir(), name+".bin")
}

func (a *App) aliasDir() string { return filepath.Join(a.dataDir, "bin") }

// ensureBinAliases 在数据目录里建 argv[0] 分流用的软链。
//
// 每次启动都重建，且必须幂等：应用升级之后安装目录的路径会变
// （/volume1/@appstore/<appid> 下带版本号的那一层），老软链会指向一个
// 已经不存在的文件，而失败症状是"还原备份时说找不到程序"。
func (a *App) ensureBinAliases() error {
	dir := a.aliasDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	target := filepath.Join(a.binDir, "redis-server.bin")
	for _, name := range []string{"redis-check-rdb", "redis-check-aof"} {
		link := filepath.Join(dir, name+".bin")
		if cur, err := os.Readlink(link); err == nil && cur == target {
			continue
		}
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("建 %s 软链失败: %w", name, err)
		}
	}
	return nil
}

// confPath 是我们生成的主配置，每次启动都重写。
func (a *App) confPath() string { return filepath.Join(a.dataDir, "redis.conf") }

// customConfPath 是留给用户的片段，在主配置末尾 include，所以优先级最高。
func (a *App) customConfPath() string { return filepath.Join(a.dataDir, "redis.custom.conf") }

// maxmemoryPolicies 是允许的淘汰策略。白名单而不是直接透传用户输入 ——
// 写错一个字母 redis-server 会拒绝启动，而错误只出现在日志里。
var maxmemoryPolicies = []string{
	"noeviction",
	"allkeys-lru", "allkeys-lfu", "allkeys-random",
	"volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl",
}

func validPolicy(p string) bool {
	for _, v := range maxmemoryPolicies {
		if v == p {
			return true
		}
	}
	return false
}

// RenderConf 生成 redis.conf 的内容。抽成纯函数是为了能直接对着字符串测 ——
// 这里写错一行的表现是"启动即退"，而单元测试比装到 NAS 上试便宜太多。
func RenderConf(cfg Config, dataBase, sock, modDir, customConf string, modules []string) string {
	var b strings.Builder
	w := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
	}

	w("# 本文件由绿联 Redis 管理壳生成，【每次启动都会被覆盖】。")
	w("# 要自定义请用同目录下的 redis.custom.conf（在本文件末尾 include，优先级更高），")
	w("# 或者直接在应用的管理页里改。")
	w("")

	if cfg.IgnoreARM64COWBug {
		w("# arm64 上 Redis 启动时会自检内核有没有那个"+
			"「MADV_FREE + fork 会在后台存盘时损坏数据」的 bug。")
		w("# 绿联的原生沙箱里这段自检【跑不起来】—— 日志原文是 Failed to test the kernel，")
		w("# 也就是「测不了」，不是「查出有 bug」。而 Redis 对这两种情况一视同仁：直接退出。")
		w("# 这一行是启动失败后由管理壳自动加上的，管理页上有说明。")
		w("ignore-warnings ARM64-COW-BUG")
		w("")
	}

	// 监听。
	//
	// 默认【只绑回环】—— 这一条刻意比 PostgreSQL 那个应用更保守。
	// Redis 是被公网扫描器盯得最紧的服务之一，历史上大量入侵就是
	// "无密码 + 绑 0.0.0.0"。NAS 上的主要用法是给同机的别的应用做缓存，
	// 填 127.0.0.1 就够；真要让局域网连，在管理页上点一下就行。
	if cfg.AllowLAN {
		w("bind * -::*")
	} else {
		w("bind 127.0.0.1 -::1")
	}
	w("port %d", cfg.Port)
	w("protected-mode yes")

	// unix socket 是管理壳自己用的那条路。放在应用私有目录（0700）里，
	// 用户改端口、改绑定地址都不影响管理页连得上。
	w("unixsocket %s", sock)
	w("unixsocketperm 700")

	w("")
	w("requirepass %s", quoteConf(cfg.Password))
	w("")

	w("dir %s", quoteConf(dataBase))
	w("dbfilename dump.rdb")
	w("databases %d", cfg.Databases)
	w("")

	// 持久化。RDB 的存盘点保留 Redis 的默认三档。
	w("save 900 1")
	w("save 300 10")
	w("save 60 10000")
	// 存盘失败就拒绝写入（Redis 默认）。盘满了还继续收数据，
	// 等于安静地把数据丢在内存里等断电。
	w("stop-writes-on-bgsave-error yes")
	if cfg.AppendOnly {
		w("appendonly yes")
		w("appendfsync everysec")
	} else {
		w("appendonly no")
	}
	w("")

	if cfg.MaxMemoryMB > 0 {
		w("maxmemory %dmb", cfg.MaxMemoryMB)
	} else {
		w("maxmemory 0")
	}
	policy := cfg.MaxMemPolicy
	if !validPolicy(policy) {
		policy = "noeviction"
	}
	w("maxmemory-policy %s", policy)
	w("")

	// 日志交给管理壳：logfile 留空 = 写 stdout，我们把 stdout/stderr 收进
	// 自己的滚动日志文件，管理页才看得到。
	w("logfile \"\"")
	w("loglevel notice")
	w("syslog-enabled no")
	w("always-show-logo no")
	w("daemonize no")
	// 【不能写 supervised systemd】：那会让 redis 去 sd_notify，
	// 而我们并不是它的 systemd 单元，结果是它等一个永远不来的确认。
	w("supervised no")
	w("")

	if cfg.LoadModules {
		for _, m := range modules {
			w("loadmodule %s", filepath.Join(modDir, m))
		}
		w("")
	}

	// include 必须是【最后一行】：Redis 是顺序读配置的，后面的同名项覆盖前面的，
	// 所以放在这里用户才真的能覆盖我们生成的任何一条。
	w("# 用户自定义片段。写在最后所以优先级最高。")
	w("include %s", customConf)
	return b.String()
}

// quoteConf 把值包成 Redis 配置能接受的双引号形式。
// 密码和路径里可能有空格或特殊字符，不加引号会被截断成半截。
func quoteConf(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// availableModules 列出包里真正存在的模块文件。
// 按固定顺序返回，配置文件才不会每次启动都长得不一样。
func (a *App) availableModules() []string {
	ents, err := os.ReadDir(a.modDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".so") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// writeConf 把配置落盘。
func (a *App) writeConf() error {
	cfg := a.config()
	if err := os.MkdirAll(filepath.Join(a.dataDir, "tmp"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(a.DataBase(), 0o700); err != nil {
		return fmt.Errorf("建数据落点 %s: %w", a.DataBase(), err)
	}
	// 用户片段不存在就建一个带说明的空文件。
	//
	// 【这个文件必须存在】，不只是为了友好：Redis 的 include 指向一个不存在的
	// 文件时是【致命错误】，服务直接起不来。所以每次写配置前都确保它在。
	if _, err := os.Stat(a.customConfPath()); os.IsNotExist(err) {
		_ = os.WriteFile(a.customConfPath(), []byte(
			"# 在这里写你自己的 Redis 配置，每行一条，例如：\n"+
				"# tcp-keepalive 60\n"+
				"# slowlog-log-slower-than 10000\n"+
				"#\n"+
				"# 这个文件在主配置末尾被 include，所以这里写的会覆盖上面的同名项。\n"+
				"# ⚠ 但【不要】在这里改 requirepass / dir / unixsocket / port / bind：\n"+
				"#    管理页不知道你改了，会连不上 Redis，页面上从此显示的都是假的。\n"+
				"#    这几项在管理页的设置卡片里都有正规入口。\n"), 0o600)
	}

	body := RenderConf(cfg, a.DataBase(), a.socketPath(), a.modDir,
		a.customConfPath(), a.availableModules())
	return os.WriteFile(a.confPath(), []byte(body), 0o600)
}

// bootRedis 是启动时的完整流程。HTTP 服务已经在跑了，所以这里慢一点没关系。
//
// 【任何一条失败路径都必须把原因写进 phase】—— 用户没有 SSH，管理页是他唯一的
// 信息来源。只在日志里 log 一句、页面上还显示"尚未启动"，等于什么都没说。
func (a *App) bootRedis() (err error) {
	defer func() {
		if err != nil {
			a.rd.setPhase("启动失败：" + err.Error())
			a.rd.setLastErr(err.Error())
		}
	}()

	if err := a.ensureBinAliases(); err != nil {
		return fmt.Errorf("准备运行环境: %w", err)
	}

	// 启动 → 起不来就按已知规则退一步再试。
	//
	// 【为什么要这套东西】：这两类失败都是 redis-server 打一行日志就退出，
	// 而那一行很容易被后面的自动重启刷掉。用户看到的只有"应用起不来"，
	// 完全不知道该往哪儿想 —— 尤其是 arm64 那条，日志里说的是内核 bug，
	// 和"我装了个 Redis"之间隔着十万八千里。
	//
	// 每条规则都是幂等的（用过之后它的 used 就为真），所以循环必然收敛。
	for attempt := 0; attempt <= len(startupFallbacks); attempt++ {
		if err := a.writeConf(); err != nil {
			return fmt.Errorf("写配置文件: %w", err)
		}
		if err := a.rd.Start(); err != nil {
			return err
		}
		if a.waitReady(25 * time.Second) {
			if note := a.fallbackNote(); note != "" {
				a.rd.setPhase("运行中（" + note + "）")
			}
			return nil
		}

		fb := a.pickFallback()
		if fb == nil {
			return fmt.Errorf("Redis 启动后没有响应 PING：%s", a.diagnoseFailure())
		}
		log.Printf("::warning:: %s", fb.why)
		// 先停：不然监管协程的退避重启会和我们的重来撞在一起。
		a.rd.Stop()
		if err := a.updateConfig(fb.apply); err != nil {
			return fmt.Errorf("%s：改配置失败: %w", fb.name, err)
		}
	}
	return fmt.Errorf("Redis 启动失败，已知的几种退让都试过了，看日志")
}

// fallbackRule 是一条"启动失败 → 改一项配置再试"的规则。
type fallbackRule struct {
	name  string
	used  func(Config) bool          // 已经用过就不再用，保证循环收敛
	match func(lines []string) bool  // 日志里是不是这条规则对应的失败
	apply func(*Config)
	why   string
}

var startupFallbacks = []fallbackRule{
	{
		name: "arm64 内核自检",
		used: func(c Config) bool { return c.IgnoreARM64COWBug },
		match: func(lines []string) bool {
			// 两个分支都要认：「测不了」和「查出有 bug」。真机上两种都见过 ——
			// 同一台机器、同一个内核，沙箱里有时报前者、有时报后者，
			// 而在沙箱【外面】跑同一个二进制则一次警告都没有。
			return logHasAny(lines, "arm64-cow-bug", "failed to test the kernel",
				"your kernel has a bug")
		},
		apply: func(c *Config) { c.IgnoreARM64COWBug = true },
		why: "Redis 在 arm64 上要自检内核的 MADV_FREE/fork bug，而这段自检在绿联沙箱里" +
			"得不出正确结果（同一个二进制在沙箱外面跑是干净通过的）。" +
			"加上 ignore-warnings ARM64-COW-BUG 再试一次。",
	},
	{
		name: "自带模块",
		used: func(c Config) bool { return !c.LoadModules },
		match: func(lines []string) bool {
			return logHasAny(lines, "can't load", "cannot load", "cannot open shared object") ||
				logHasBoth(lines, "module", "error")
		},
		apply: func(c *Config) {
			c.LoadModules = false
			c.ModulesDisabledByFallback = true
		},
		why: "Redis 因为模块加载失败没起来，自动关掉自带的四个模块再试一次",
	},
}

// diagnoseFailure 把日志里那句真正有用的话捞出来放进 phase。
//
// 【"没有响应 PING"这句话对用户毫无价值】—— 他不知道该看哪儿，也没有 SSH。
// 端口被占是这里最常见的一种，而且在 UGOS 上尤其容易踩：系统自己就占着 6379。
func (a *App) diagnoseFailure() string {
	lines := tailFile(a.logFile, 200)
	cfg := a.config()
	if logHasAny(lines, "address already in use", "could not create server tcp listening socket") {
		return fmt.Sprintf("端口 %d 已经被别的程序占用了 —— 换一个端口再试。"+
			"（注意 UGOS 系统自己就占着 6379。）", cfg.Port)
	}
	if logHasAny(lines, "permission denied") {
		return "有一步被拒绝了（权限不足），日志里有具体是哪一步"
	}
	if logHasAny(lines, "no space left") {
		return "磁盘满了"
	}
	// 捞不出已知模式就把日志最后一行非空的内容原样带出来，
	// 总比一句"看日志"强。
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return "日志最后一行是：" + s
		}
	}
	return "日志里什么都没有"
}

func (a *App) pickFallback() *fallbackRule {
	lines := tailFile(a.logFile, 200)
	cfg := a.config()
	for i := range startupFallbacks {
		fb := &startupFallbacks[i]
		if fb.used(cfg) {
			continue
		}
		if fb.match(lines) {
			return fb
		}
	}
	return nil
}

// fallbackNote 给管理页一句话，说明当前是在什么"退让"下跑着的。
func (a *App) fallbackNote() string {
	cfg := a.config()
	var parts []string
	if cfg.ModulesDisabledByFallback {
		parts = append(parts, "自带模块加载失败已关闭")
	}
	if cfg.IgnoreARM64COWBug {
		parts = append(parts, "已跳过 arm64 内核自检")
	}
	return strings.Join(parts, "；")
}

func logHasAny(lines []string, needles ...string) bool {
	for _, ln := range lines {
		low := strings.ToLower(ln)
		for _, n := range needles {
			if strings.Contains(low, n) {
				return true
			}
		}
	}
	return false
}

func logHasBoth(lines []string, a, b string) bool {
	for _, ln := range lines {
		low := strings.ToLower(ln)
		if strings.Contains(low, a) && strings.Contains(low, b) {
			return true
		}
	}
	return false
}

// waitReady 等 Redis 真的能接受命令。
func (a *App) waitReady(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !a.rd.Status().Running {
			// 进程已经没了，再等也没意义
			time.Sleep(300 * time.Millisecond)
			if !a.rd.Status().Running {
				return false
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := a.rds.Do(ctx, "PING")
		cancel()
		if err == nil {
			a.rd.setPhase("运行中")
			a.rd.clearFails()
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}


// ---- 对 Redis 的常用操作 -------------------------------------------------

func (a *App) ping(ctx context.Context) bool {
	_, err := a.rds.Do(ctx, "PING")
	return err == nil
}

// info 取一段 INFO。section 为空表示默认全部。
func (a *App) info(ctx context.Context, section string) (map[string]string, error) {
	args := []string{"INFO"}
	if section != "" {
		args = append(args, section)
	}
	v, err := a.rds.Do(ctx, args...)
	if err != nil {
		return nil, err
	}
	return ParseInfo(v.Str), nil
}

// serverVersion 返回形如 "8.10.0" 的版本号。
func (a *App) serverVersion(ctx context.Context) string {
	info, err := a.info(ctx, "server")
	if err != nil {
		return ""
	}
	return info["redis_version"]
}

// bgsave 触发一次后台存盘，并等它真的完成。
//
// 【必须等】：BGSAVE 是立刻返回的，回来时 dump.rdb 还在写。备份功能如果
// 拿这一刻的文件去复制，得到的是半截 RDB —— 而它看起来是个正常文件，
// 直到有人拿它去还原。靠 rdb_bgsave_in_progress + rdb_last_save_time 判断。
func (a *App) bgsave(ctx context.Context) error {
	before, err := a.lastSave(ctx)
	if err != nil {
		return err
	}
	if _, err := a.rds.Do(ctx, "BGSAVE"); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待存盘完成超时：%w", ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
		info, err := a.info(ctx, "persistence")
		if err != nil {
			return err
		}
		if info["rdb_bgsave_in_progress"] == "1" {
			continue
		}
		now, err := a.lastSave(ctx)
		if err != nil {
			return err
		}
		if now != before {
			if info["rdb_last_bgsave_status"] != "ok" {
				return fmt.Errorf("Redis 报告存盘失败（rdb_last_bgsave_status=%s），"+
					"多半是磁盘满了或没有写权限", info["rdb_last_bgsave_status"])
			}
			return nil
		}
	}
}

func (a *App) lastSave(ctx context.Context) (int64, error) {
	v, err := a.rds.Do(ctx, "LASTSAVE")
	if err != nil {
		return 0, err
	}
	return v.Int, nil
}

// rdbPath 是当前的 dump.rdb。
func (a *App) rdbPath() string { return filepath.Join(a.DataBase(), "dump.rdb") }

// dangerousCommands 是管理页上的「运行命令」要拦下来的。
//
// 【拦的不是"危险"，是"会把管理壳自己搞糊涂"】：
//   - SHUTDOWN / CONFIG SET dir / CONFIG SET requirepass / CONFIG SET appendonly …
//     这些会让实际状态和管理壳的 config.json 分家，之后管理页显示的全是假的，
//     而且下次重启又被配置文件改回去 —— 用户会觉得"设了没反应"。
//     这些事都有正规入口（设置卡片），走那边才会两边同时更新。
//   - MONITOR / SUBSCRIBE 这类【不会返回】的命令会把我们那条连接挂死。
//   - DEBUG SLEEP 同理。
//
// 其余命令一律放行：能进到这个页面的已经是 NAS 管理员，拦 FLUSHALL 这种
// 只会让人绕路，没有实际安全收益。
var blockedCommands = map[string]string{
	"SHUTDOWN":  "请用页面上的「停止」按钮，那样管理壳才知道是主动停的、不会自动把它拉起来",
	"MONITOR":   "这条命令不会返回，会把管理壳和 Redis 之间的连接挂死",
	"SUBSCRIBE": "这条命令会一直阻塞，管理页用不了它",
	"PSUBSCRIBE": "这条命令会一直阻塞，管理页用不了它",
	"SSUBSCRIBE": "这条命令会一直阻塞，管理页用不了它",
	"RESET":     "会把连接状态重置掉，管理壳的认证会跟着失效",
	"HELLO":     "会切换协议版本，管理壳只说 RESP2",
}

// blockedConfigSet 是 CONFIG SET 里不许改的项：它们在管理页上都有正规入口，
// 从命令行改会造成"页面显示的"和"实际生效的"两套事实。
var blockedConfigSet = map[string]string{
	"requirepass": "改密码请用设置卡片，那样管理壳才会同步更新自己的连接凭据",
	"dir":         "改数据目录请用「迁移数据目录」，直接改会让数据散落在两个地方",
	"appendonly":  "请用设置卡片里的 AOF 开关",
	"maxmemory":   "请用设置卡片里的内存上限",
	"port":        "请用设置卡片里的端口",
	"unixsocket":  "这条是管理壳自己连 Redis 用的，改了页面就连不上了",
	"bind":        "请用设置卡片里的「允许局域网连接」",
}

// checkCommand 在发出去之前挡一遍。
func checkCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("空命令")
	}
	name := strings.ToUpper(args[0])
	if why, bad := blockedCommands[name]; bad {
		return fmt.Errorf("%s 不能从这里执行：%s", name, why)
	}
	if name == "CONFIG" && len(args) >= 3 && strings.EqualFold(args[1], "SET") {
		if why, bad := blockedConfigSet[strings.ToLower(args[2])]; bad {
			return fmt.Errorf("不能在这里 CONFIG SET %s：%s", args[2], why)
		}
	}
	if name == "DEBUG" && len(args) >= 2 && strings.EqualFold(args[1], "SLEEP") {
		return fmt.Errorf("DEBUG SLEEP 会阻塞整个 Redis，不要从管理页发它")
	}
	return nil
}

// humanBytesStr 把 INFO 里的字节数变成好读的形式（那些值都是十进制字符串）。
func humanBytesStr(s string) string {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return s
	}
	return humanBytes(n)
}
