// Command redisshell 是 Redis 在绿联 UGOS Pro 上的管理壳。
//
// 为什么需要这么一层：绿联要求应用在 project.yaml 声明的 port 上【提供 HTTP 服务】
// （桌面图标就是打开它，系统也靠它探活），而 Redis 只会说 RESP。
// 所以 start_cmd 指向这个 Go 程序，由它：
//   - 生成 redis.conf，把 redis-server 当子进程拉起来并守护；
//   - 在自己的端口上提供一个管理页（状态 / 连接信息 / 内存与命中率 / 备份 /
//     改密码 / 运行命令 / 日志 / 数据目录迁移）；
//   - 直接说 RESP 和 Redis 通信（见 resp.go），不打包也不 fork redis-cli。
//
// 沙箱相关的前提只有一条（比 PostgreSQL 那个应用少两条，见 scripts/fetch-runtime.py）：
// /usr 不存在，所以整条依赖闭包自带，用显式 loader 启动。
package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type App struct {
	// 路径
	appRoot  string // 应用安装目录（launcher 二进制的上一级）
	redisDir string // <appRoot>/redis
	binDir   string // <redisDir>/bin
	runtime  string // <redisDir>/lib/runtime
	modDir   string // <redisDir>/modules
	dataDir  string // UGAPP_DATA_DIR
	sockDir  string // <dataDir>/run
	uploads  string // <dataDir>/uploads
	backups  string // <dataDir>/backups
	logFile  string // <dataDir>/logs/redis.log
	cfgPath  string // <dataDir>/config.json

	// dataBase 是 Redis 数据文件（dump.rdb / appendonlydir）的落点。
	// 迁移时会在运行中被换掉，所以带锁。
	baseMu   sync.RWMutex
	dataBase string

	cfgMu sync.RWMutex
	cfg   *Config

	mig migrateState

	rds *Client
	rd  *Supervisor
	rec *Recorder // launcher 自己的日志环形缓冲，供管理页显示
}

// DataBase 返回 Redis 数据文件的落点（redis.conf 里的 dir）。
func (a *App) DataBase() string {
	a.baseMu.RLock()
	defer a.baseMu.RUnlock()
	return a.dataBase
}

func (a *App) setDataBase(dir string) {
	a.baseMu.Lock()
	a.dataBase = dir
	a.baseMu.Unlock()
}

// Config 是管理壳自己的配置，存 <dataDir>/config.json（0600）。
//
// 注意这里【存了明文的 Redis 密码】。这是刻意的取舍：用户要拿这个密码去配
// 别的应用，看不到就没法用；而这个文件在应用私有数据目录里、权限 0600、
// 沙箱本身又是隔离的。和 docker 把 REDIS_PASSWORD 放在环境变量里是同一个量级。
//
// 这里【没有管理页的密码】：管理页的鉴权完全交给 UGOS 的登录认证能力
// （网关校验 + 注入用户头，见 ugauth.go），应用自己不再实现账号系统。
type Config struct {
	Password string `json:"password"`  // requirepass
	Port     int    `json:"port"`      // Redis 端口
	AllowLAN bool   `json:"allow_lan"` // 是否绑 0.0.0.0

	// AOF：默认【开】。这是刻意偏离 Redis 上游默认值（appendonly no）的。
	// 理由是场景不同：NAS 断电是常态，而只有 RDB 的话默认存盘点最长要 15 分钟
	// （save 900 1），断电就丢 15 分钟的写入。对一个被叫做"数据库"的东西来说
	// 这是会让人吃惊的行为。everysec 最多丢 1 秒。
	AppendOnly bool `json:"appendonly"`

	// 0 = 不限制（Redis 上游默认）。管理页上会显示本机内存并提示。
	MaxMemoryMB  int    `json:"maxmemory_mb"`
	MaxMemPolicy string `json:"maxmemory_policy"`

	// Redis 8 自带的四个模块（JSON / 搜索 / 时序 / 布隆）。默认加载，
	// 和上游 deb 的行为一致；小内存机器可以关掉，光 redisearch 就 15MB。
	LoadModules bool `json:"load_modules"`

	// Databases 是 SELECT 能用的库数量，Redis 默认 16。
	Databases int `json:"databases"`

	// DataBase 是数据文件落点，初始化那一刻定下、此后不跟着参数漂。
	// 理由和 PostgreSQL 那个应用一样：安装参数首次启动读到的是空值
	// （平台先起服务、2~3 秒后才写 .env，而 .env 应用自己读不到），
	// 每次启动重算会让数据"第一次落在 A、重启后落在 B"。
	DataBase string `json:"data_base"`

	// ModulesDisabledByFallback 记录"因为模块加载失败而自动关掉了模块"。
	// 只用来在管理页上说清楚为什么开关和实际不一致。
	ModulesDisabledByFallback bool `json:"modules_disabled_by_fallback"`

	// IgnoreARM64COWBug 对应配置项 `ignore-warnings ARM64-COW-BUG`。
	//
	// arm64 上 Redis 启动时会跑一段自检，确认内核没有那个"MADV_FREE + fork
	// 会在后台存盘时损坏数据"的 bug。绿联的原生沙箱里这段自检【跑不起来】
	// （不是"查出有 bug"，是"测不了"），而 Redis 对这两种情况一视同仁：直接退出。
	//
	// 所以只能告诉它跳过。这一位由启动失败时的自动回退置上并落盘，
	// 之后每次启动直接带着 —— 不然每次开机都要先失败一次。
	// 管理页上会用横幅把这件事说清楚，不偷偷做。
	IgnoreARM64COWBug bool `json:"ignore_arm64_cow_bug"`
}

// defaultPort 不用 Redis 上游的 6379。
//
// 【UGOS 自己就跑着一个 Redis 在 127.0.0.1:6379】（真机实测：
// /usr/bin/redis-server 127.0.0.1:6379，系统服务）。照抄上游默认端口的结果是
// 我们的 redis 绑不上、启动即退，日志里只有一行 Address already in use ——
// 而用户完全不知道"端口被系统占了"这回事。
//
// 16379 = 6379 前面加个 1，好记，真机上确认没人占。
const defaultPort = 16379

func defaultConfig() *Config {
	return &Config{
		Password:     randomPassword(24),
		Port:         defaultPort,
		AllowLAN:     false, // 见下面 writeConf 里的说明：Redis 默认不出局域网
		AppendOnly:   true,
		MaxMemoryMB:  0,
		MaxMemPolicy: "noeviction",
		LoadModules:  true,
		Databases:    16,
	}
}

func main() {
	port := flag.Int("port", 26379, "管理页监听端口")
	flag.Parse()

	log.SetFlags(log.LstdFlags)
	app, err := newApp()
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	log.SetOutput(app.rec) // rec 同时写 stderr 和环形缓冲

	// HTTP 先起来，再去启动 Redis。绿联会拿这个端口探活 ——
	// 让探活先通过，页面上再显示"正在启动"，比让系统以为应用起不来好得多。
	srv := app.startHTTP(*port)

	go func() {
		app.housekeeping()
		if err := app.bootRedis(); err != nil {
			log.Printf("Redis 启动失败: %v", err)
		}
	}()
	go app.gcLoop()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	sig := <-stop
	log.Printf("收到信号 %v，开始关闭", sig)

	app.shutdownHTTP(srv)
	app.rd.Stop()
	app.rds.Close()
	log.Printf("已退出")
}

func newApp() (*App, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("定位自身路径: %w", err)
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	appRoot := filepath.Dir(filepath.Dir(exe)) // <appRoot>/bin/launcher

	dataDir := os.Getenv("UGAPP_DATA_DIR")
	if dataDir == "" {
		// 本机开发时用当前目录下的 data/，方便完全脱离 NAS 跑通前后端。
		dataDir, _ = filepath.Abs("data")
	}

	a := &App{
		appRoot:  appRoot,
		redisDir: filepath.Join(appRoot, "redis"),
		dataDir:  dataDir,
		rec:      newRecorder(4000),
	}
	a.binDir = filepath.Join(a.redisDir, "bin")
	a.runtime = filepath.Join(a.redisDir, "lib", "runtime")
	a.modDir = filepath.Join(a.redisDir, "modules")
	a.sockDir = filepath.Join(dataDir, "run")
	a.uploads = filepath.Join(dataDir, "uploads")
	a.backups = filepath.Join(dataDir, "backups")
	a.logFile = filepath.Join(dataDir, "logs", "redis.log")
	a.cfgPath = filepath.Join(dataDir, "config.json")

	for _, d := range []string{a.sockDir, a.uploads, a.backups, filepath.Dir(a.logFile)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("建目录 %s: %w", d, err)
		}
	}
	if err := a.loadConfig(); err != nil {
		return nil, err
	}
	if err := a.resolveDataBase(); err != nil {
		return nil, err
	}

	cfg := a.config()
	a.rds = NewClient("unix", a.socketPath(), cfg.Password)
	a.rd = newSupervisor(a)
	return a, nil
}

// socketPath 是管理壳和 Redis 之间那条 unix socket。
//
// 【走 unix socket 而不是 127.0.0.1】：这样管理页在用户把 Redis 端口改到
// 任何地方、甚至只绑回环之外的地址时都还连得上，也不占一个 TCP 端口。
// 路径放在应用私有数据目录（0700）里，同机的别的应用也进不来。
func (a *App) socketPath() string { return filepath.Join(a.sockDir, "redis.sock") }

func (a *App) loadConfig() error {
	buf, err := os.ReadFile(a.cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		a.cfg = defaultConfig()
		return a.saveConfig()
	}
	if err != nil {
		return fmt.Errorf("读配置: %w", err)
	}
	cfg := defaultConfig()
	if err := json.Unmarshal(buf, cfg); err != nil {
		return fmt.Errorf("解析配置 %s: %w", a.cfgPath, err)
	}
	a.cfg = cfg
	return nil
}

func (a *App) saveConfig() error {
	a.cfgMu.RLock()
	buf, err := json.MarshalIndent(a.cfg, "", "  ")
	a.cfgMu.RUnlock()
	if err != nil {
		return err
	}
	tmp := a.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.cfgPath)
}

// config 读一份快照。所有 handler 都用这个，避免到处加锁。
func (a *App) config() Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return *a.cfg
}

// updateConfig 在锁内改配置并落盘。
func (a *App) updateConfig(fn func(*Config)) error {
	a.cfgMu.Lock()
	fn(a.cfg)
	a.cfgMu.Unlock()
	return a.saveConfig()
}

// ---- 数据落点 ------------------------------------------------------------

// paramDataDir 读安装参数 REDIS_DATA_DIR。
//
// 空字符串和字面量 "null" 都算"没填"：平台在用户没选目录时写进 .env 的就是
// 后者（不是空串），照字面用会建出一个名叫 null 的目录。
func paramDataDir() string {
	v := strings.TrimSpace(os.Getenv("REDIS_DATA_DIR"))
	if v == "" || v == "null" {
		return ""
	}
	return v
}

// dataSubdir：数据不直接铺在用户选的共享文件夹根上，放进这个子目录。
const dataSubdir = "redis-data"

// DataBaseFromParam 把安装参数里的共享文件夹换算成真正的数据落点。
// 【首次初始化和迁移必须用同一个函数】，否则同一个参数值在两条路上会指到不同地方。
func DataBaseFromParam(param string) string {
	return filepath.Join(param, dataSubdir)
}

func (a *App) resolveDataBase() error {
	if base := a.config().DataBase; base != "" {
		a.setDataBase(base)
		return os.MkdirAll(base, 0o700)
	}

	param := paramDataDir()
	chosen := ""
	if param != "" {
		candidate := DataBaseFromParam(param)
		if err := os.MkdirAll(candidate, 0o700); err == nil {
			chosen = candidate
		} else {
			log.Printf("安装参数指定的目录 %s 不可写（%v），改用应用数据目录", param, err)
		}
	}
	if chosen == "" {
		chosen = filepath.Join(a.dataDir, "data")
		if err := os.MkdirAll(chosen, 0o700); err != nil {
			return fmt.Errorf("创建数据目录 %s 失败: %w", chosen, err)
		}
	}
	a.setDataBase(chosen)
	return a.updateConfig(func(c *Config) { c.DataBase = chosen })
}

// dataBaseParamChanged 判断"用户后来改了安装参数，但数据还在老地方"。
// 只用来在管理页上提示 —— 搬数据要停服，必须由人点一次才动。
func (a *App) dataBaseParamChanged() (bool, string) {
	now := paramDataDir()
	if now == "" {
		return false, ""
	}
	target := DataBaseFromParam(now)
	if filepath.Clean(target) == filepath.Clean(a.DataBase()) {
		return false, ""
	}
	return true, target
}

// ---- 杂务 ---------------------------------------------------------------

// housekeeping 清理上次运行留下的、留着只有坏处的东西。必须幂等。
func (a *App) housekeeping() {
	// 上次没退干净留下的 socket 文件。Redis 启动时发现路径已存在会直接失败，
	// 表现是"启动即退"而日志里只有一行 bind 错误。
	if err := os.Remove(a.socketPath()); err == nil {
		log.Printf("清理了上次残留的 socket 文件 %s", a.socketPath())
	}
	a.gcUploads()
}

func (a *App) gcLoop() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		a.gcUploads()
	}
}

// randomPassword 生成一个便于人工抄写的随机密码：
// 去掉了 0/O/1/l/I 这些容易看错的字符，用户要把它抄进别的应用的配置里。
func randomPassword(n int) string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var sb strings.Builder
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			// crypto/rand 失败在 Linux 上基本不可能；真发生了也绝不能退回弱随机。
			panic(fmt.Sprintf("crypto/rand 不可用: %v", err))
		}
		sb.WriteByte(alphabet[idx.Int64()])
	}
	return sb.String()
}
