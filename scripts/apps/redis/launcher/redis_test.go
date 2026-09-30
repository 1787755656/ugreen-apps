package main

import (
	"fmt"
	"strings"
	"testing"
)

func render(cfg Config) string {
	return RenderConf(cfg, "/data/redis-data", "/data/run/redis.sock",
		"/app/redis/modules", "/data/redis.custom.conf",
		[]string{"redisbloom.so", "rejson.so"})
}

// 配置写错一行的表现是"启动即退，日志里只有一句看不懂的话"。
// 对着字符串测比装到 NAS 上试便宜太多。
func TestRenderConf(t *testing.T) {
	cfg := *defaultConfig()
	cfg.Password = "pw"
	out := render(cfg)

	must := []string{
		fmt.Sprintf("port %d", defaultPort),
		"unixsocket /data/run/redis.sock",
		"unixsocketperm 700",
		`dir "/data/redis-data"`,
		"daemonize no",
		"supervised no",
		`logfile ""`,
		"stop-writes-on-bgsave-error yes",
	}
	for _, s := range must {
		if !strings.Contains(out, s) {
			t.Errorf("配置里缺少 %q", s)
		}
	}
}

// 默认【只绑回环】。Redis 是被公网扫描器盯得最紧的服务之一，
// 历史上大量入侵就是"无密码 + 绑 0.0.0.0"。这条默认值不能被无意改掉。
func TestRenderConfBindsLoopbackByDefault(t *testing.T) {
	cfg := *defaultConfig()
	if cfg.AllowLAN {
		t.Fatal("AllowLAN 的默认值必须是 false")
	}
	out := render(cfg)
	if !strings.Contains(out, "bind 127.0.0.1 -::1") {
		t.Error("默认应当只绑回环")
	}
	if strings.Contains(out, "bind * ") {
		t.Error("默认配置里不该出现绑全部地址")
	}

	cfg.AllowLAN = true
	out = render(cfg)
	if !strings.Contains(out, "bind * -::*") {
		t.Error("打开局域网访问后应当绑全部地址")
	}
	// 无论如何 protected-mode 都要在
	if !strings.Contains(out, "protected-mode yes") {
		t.Error("protected-mode 必须一直是 yes")
	}
}

// 密码和路径必须带引号：里面有空格或 # 的话，不加引号会被 Redis 截成半截，
// 结果是"密码不对"或者"数据写到了别的地方"，都很难往配置格式上想。
func TestRenderConfQuotesValues(t *testing.T) {
	cfg := *defaultConfig()
	cfg.Password = `a b"c#d`
	out := render(cfg)
	if !strings.Contains(out, `requirepass "a b\"c#d"`) {
		t.Fatalf("密码没有被正确转义：\n%s", out)
	}
}

// include 必须是最后一行 —— Redis 顺序读配置、后面的覆盖前面的，
// 放在中间的话用户在自定义片段里写的东西会被我们生成的行盖掉，
// 表现就是"改了没反应"。
func TestRenderConfIncludeIsLast(t *testing.T) {
	out := strings.TrimRight(render(*defaultConfig()), "\n")
	lines := strings.Split(out, "\n")
	last := lines[len(lines)-1]
	if last != "include /data/redis.custom.conf" {
		t.Fatalf("最后一行是 %q，应当是 include", last)
	}
}

func TestRenderConfModules(t *testing.T) {
	cfg := *defaultConfig()
	cfg.LoadModules = true
	out := render(cfg)
	if !strings.Contains(out, "loadmodule /app/redis/modules/redisbloom.so") {
		t.Error("开了模块却没有 loadmodule 行")
	}
	cfg.LoadModules = false
	out = render(cfg)
	if strings.Contains(out, "loadmodule") {
		t.Error("关了模块还有 loadmodule 行")
	}
}

func TestRenderConfMemoryAndAOF(t *testing.T) {
	cfg := *defaultConfig()
	// 默认开 AOF：NAS 断电是常态，只有 RDB 的话最长会丢 15 分钟的写入
	if !cfg.AppendOnly {
		t.Fatal("AOF 默认应当是开的")
	}
	out := render(cfg)
	if !strings.Contains(out, "appendonly yes") || !strings.Contains(out, "appendfsync everysec") {
		t.Error("AOF 配置不对")
	}

	cfg.AppendOnly = false
	if out = render(cfg); !strings.Contains(out, "appendonly no") {
		t.Error("关掉 AOF 后配置不对")
	}

	cfg.MaxMemoryMB = 512
	if out = render(cfg); !strings.Contains(out, "maxmemory 512mb") {
		t.Error("内存上限没写进去")
	}
	cfg.MaxMemoryMB = 0
	if out = render(cfg); !strings.Contains(out, "maxmemory 0") {
		t.Error("0 应当写成 maxmemory 0（不限制）")
	}

	// 非法策略要退回安全值，绝不能原样透传 —— 写错一个字母 redis-server 拒绝启动
	cfg.MaxMemPolicy = "allkeys-lru; rm -rf /"
	if out = render(cfg); !strings.Contains(out, "maxmemory-policy noeviction") {
		t.Error("非法的淘汰策略应当退回 noeviction")
	}
}

// 真机上第一次装就撞上了：arm64 的绿联沙箱里 Redis 那段 MADV_FREE/fork 内核自检
// 【跑不起来】，而 Redis 对"测不了"和"查出有 bug"一视同仁 —— 打一行日志就退出。
// 日志原文：Failed to test the kernel for a bug that could lead to data corruption…
func TestARM64COWFallback(t *testing.T) {
	cfg := *defaultConfig()
	// 默认不该带这一行：amd64 和正常内核上根本用不到它，
	// 无脑加等于把一条真实的数据保护措施永久关掉。
	if cfg.IgnoreARM64COWBug {
		t.Fatal("默认不该跳过内核自检")
	}
	if strings.Contains(render(cfg), "ignore-warnings") {
		t.Fatal("默认配置里不该出现 ignore-warnings")
	}

	cfg.IgnoreARM64COWBug = true
	if !strings.Contains(render(cfg), "ignore-warnings ARM64-COW-BUG") {
		t.Fatal("置位之后应当写出 ignore-warnings ARM64-COW-BUG")
	}
}

// 默认端口不能是 6379：UGOS 系统自己就跑着一个 Redis 占着它
// （真机实测 /usr/bin/redis-server 127.0.0.1:6379）。
// 照抄上游默认值的后果是我们的 redis 绑不上、启动即退。
func TestDefaultPortAvoidsSystemRedis(t *testing.T) {
	if defaultConfig().Port == 6379 {
		t.Fatal("默认端口不能用 6379 —— UGOS 系统自带的 Redis 已经占了")
	}
	if p := defaultConfig().Port; p < 1024 || p > 65535 {
		t.Fatalf("默认端口 %d 不合法", p)
	}
}

// 回退规则要认得出日志、而且【只用一次】—— 否则启动循环不会收敛。
func TestStartupFallbackRules(t *testing.T) {
	cowLog := []string{
		"995396:C 09 Aug 2026 13:45:09.133 # Failed to test the kernel for a bug that " +
			"could lead to data corruption during background save.",
		"995396:C 09 Aug 2026 13:45:09.133 # Redis will now exit to prevent data corruption. " +
			"Note that it is possible to suppress this warning by setting the following " +
			"config: ignore-warnings ARM64-COW-BUG",
	}
	modLog := []string{
		"1:M 09 Aug 2026 13:45:09.133 # Can't load module from /app/redis/modules/redisearch.so: " +
			"libstdc++.so.6: cannot open shared object file",
	}

	find := func(lines []string, cfg Config) *fallbackRule {
		for i := range startupFallbacks {
			fb := &startupFallbacks[i]
			if !fb.used(cfg) && fb.match(lines) {
				return fb
			}
		}
		return nil
	}

	fresh := *defaultConfig()

	fb := find(cowLog, fresh)
	if fb == nil || fb.name != "arm64 内核自检" {
		t.Fatalf("没认出 arm64 内核自检那条日志，got %v", fb)
	}
	// 用过之后必须不再命中，否则 bootRedis 会一直重试同一条
	after := fresh
	fb.apply(&after)
	if find(cowLog, after) != nil {
		t.Fatal("同一条规则被选了第二次 —— 启动循环不会收敛")
	}

	fb = find(modLog, fresh)
	if fb == nil || fb.name != "自带模块" {
		t.Fatalf("没认出模块加载失败那条日志，got %v", fb)
	}
	after = fresh
	fb.apply(&after)
	if !after.ModulesDisabledByFallback || after.LoadModules {
		t.Fatal("模块回退没有正确置位")
	}
	if find(modLog, after) != nil {
		t.Fatal("模块规则被选了第二次")
	}

	// 无关的日志不能触发任何回退 —— 误触发会把模块或内核自检白白关掉
	quiet := []string{"1:M 09 Aug 2026 13:45:09.133 * Ready to accept connections tcp"}
	if fb := find(quiet, fresh); fb != nil {
		t.Fatalf("正常日志不该触发回退，却选中了 %s", fb.name)
	}
}

func TestValidPolicy(t *testing.T) {
	for _, p := range maxmemoryPolicies {
		if !validPolicy(p) {
			t.Errorf("%q 应当合法", p)
		}
	}
	for _, p := range []string{"", "lru", "allkeys", "ALLKEYS-LRU", "noeviction "} {
		if validPolicy(p) {
			t.Errorf("%q 不该被接受", p)
		}
	}
}

// 挡下来的不是"危险命令"，是"会让管理壳和实际状态分家"或者"会把连接挂死"的那些。
func TestCheckCommand(t *testing.T) {
	blocked := [][]string{
		{"SHUTDOWN"},
		{"shutdown", "nosave"},
		{"MONITOR"},
		{"SUBSCRIBE", "ch"},
		{"CONFIG", "SET", "requirepass", "x"},
		{"config", "set", "dir", "/tmp"},
		{"CONFIG", "SET", "appendonly", "no"},
		{"DEBUG", "SLEEP", "100"},
	}
	for _, args := range blocked {
		if err := checkCommand(args); err == nil {
			t.Errorf("%v 应当被挡下来", args)
		}
	}

	// 这些必须放行：能进到这个页面的已经是 NAS 管理员，
	// 拦 FLUSHALL 这种只会让人绕路，没有实际安全收益。
	allowed := [][]string{
		{"GET", "k"},
		{"SET", "k", "v"},
		{"FLUSHALL"},
		{"KEYS", "*"},
		{"CONFIG", "GET", "maxmemory"},
		{"CONFIG", "SET", "slowlog-log-slower-than", "10000"},
		{"INFO"},
		{"CLIENT", "LIST"},
	}
	for _, args := range allowed {
		if err := checkCommand(args); err != nil {
			t.Errorf("%v 不该被挡：%v", args, err)
		}
	}

	if err := checkCommand(nil); err == nil {
		t.Error("空命令应当报错")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KiB",
		1536: "1.5 KiB", 1 << 20: "1.0 MiB", 3 << 30: "3.0 GiB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
	// INFO 里的值是十进制字符串；解析不了就原样返回，不能变成 0
	if got := humanBytesStr("1048576"); got != "1.0 MiB" {
		t.Errorf("humanBytesStr = %q", got)
	}
	if got := humanBytesStr(""); got != "" {
		t.Errorf("空值应当原样返回，got %q", got)
	}
}

func TestSummarizeHitRate(t *testing.T) {
	s := summarize(map[string]string{"keyspace_hits": "75", "keyspace_misses": "25"})
	if s["hit_rate"] != "75.0%" {
		t.Errorf("命中率 = %q", s["hit_rate"])
	}
	// 一次都没查过的时候不能显示成 0%（那看起来像"缓存完全没用上"）
	s = summarize(map[string]string{"keyspace_hits": "0", "keyspace_misses": "0"})
	if s["hit_rate"] != "—" {
		t.Errorf("没有样本时命中率应当是 —，got %q", s["hit_rate"])
	}
}
