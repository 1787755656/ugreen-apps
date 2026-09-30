// Command pgshell 是 PostgreSQL 在绿联 UGOS Pro 上的管理壳。
//
// 为什么需要这么一层：绿联要求应用在 project.yaml 声明的 port 上【提供 HTTP 服务】
// （桌面图标就是打开它，系统也靠它探活），而 PostgreSQL 只会说 wire protocol。
// 所以 start_cmd 指向这个 Go 程序，由它：
//   - 首次运行时 initdb，之后每次启动把 postgres 当子进程拉起来并守护；
//   - 在自己的端口上提供一个管理页（状态 / 连接信息 / 改密 / 数据库增删 / 备份 / 日志）；
//   - 把沙箱缺的东西补上（时区目录软链、nss_wrapper 的 passwd 文件）。
//
// 沙箱相关的三个前提（详见 scripts/fetch-runtime.py 的文件头）：
//  1. 自带整条依赖闭包，bin/ 下每个可执行文件都是"sh 包装脚本 + .bin 真身"；
//  2. postgres/initdb 里的时区目录被改成了【相对路径 ../zoneinfo】，
//     所以【每个子进程的 cwd 都必须由我们显式指定】，见 pgCommand()；
//  3. 沙箱没有 /etc/passwd，getpwuid() 查不到会让 initdb 直接退出，
//     靠 LD_PRELOAD nss_wrapper 顶上。
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

// 运行 PostgreSQL 的用户名。沙箱里的真实系统用户是 app_xxxx 这种随机名字，
// 但我们用 nss_wrapper 把 getpwuid() 的结果固定成 postgres，于是：
//   - initdb 默认建出来的超级用户就叫 postgres（和所有人的预期一致）；
//   - unix socket 的 peer 认证也对得上。
const pgUser = "postgres"

// PostgreSQL 大版本。要跟 fetch-runtime.py 的 --pg-major 一致 ——
// 目录布局 lib/postgresql/<major>/bin 是 PostgreSQL 自己推导 share 路径的依据。
const pgMajor = "17"

type App struct {
	// 路径
	appRoot string // 应用安装目录（launcher 二进制的上一级）
	pgRoot  string // <appRoot>/pgsql
	binDir  string // <pgRoot>/lib/postgresql/17/bin
	runtime string // <pgRoot>/lib/runtime
	dataDir string // UGAPP_DATA_DIR
	initCwd string // <dataDir>/init-cwd
	sockDir string // <dataDir>/run
	nssDir  string // <dataDir>/nss
	uploads string // <dataDir>/uploads
	backups string // <dataDir>/backups
	logFile string // <dataDir>/logs/postgresql.log
	cfgPath string // <dataDir>/config.json

	// pgBase 是数据库文件的落点，PGDATA 是它下面的 pgdata 子目录。
	//
	// 【为什么是"上一级"而不是直接指 PGDATA】：postgres 二进制里的时区目录被我们
	// 改成了相对路径 ../zoneinfo（见 README），而 postgres 的 cwd 就是 PGDATA。
	// 也就是说 PGDATA 的【同级】必须有一个 zoneinfo 软链，否则 postgres 连
	// postgresql.conf 都读不到就退出。所以数据搬到哪儿，这个"容器目录"就得跟到哪儿。
	//
	// 迁移时会在运行中被换掉，所以带锁。
	pgBaseMu sync.RWMutex
	pgBase   string

	cfgMu sync.RWMutex
	cfg   *Config

	// mig 是迁移的进度与互斥。同一时刻只允许一个迁移在跑 ——
	// 迁移会停库、搬几个 G 的文件，两个并发的迁移能把数据搅成任何样子。
	mig migrateState

	pg  *Supervisor
	rec *Recorder // launcher 自己的日志环形缓冲，供管理页显示
}

// PGBase 返回当前的数据落点（PGDATA 的上一级）。
func (a *App) PGBase() string {
	a.pgBaseMu.RLock()
	defer a.pgBaseMu.RUnlock()
	return a.pgBase
}

// PGData 返回当前的 PGDATA。
func (a *App) PGData() string {
	return filepath.Join(a.PGBase(), pgDataSubdir)
}

func (a *App) setPGBase(dir string) {
	a.pgBaseMu.Lock()
	a.pgBase = dir
	a.pgBaseMu.Unlock()
}

// pgDataSubdir 是 PGDATA 在 pgBase 下的名字。
// 迁移前后必须一致，否则同一份数据在两个位置的路径规则不一样。
const pgDataSubdir = "pgdata"

// Config 是 launcher 自己的配置，存 <dataDir>/config.json（0600）。
//
// 注意这里【存了明文的数据库密码】。这是刻意的取舍：用户要拿这个密码去配
// Immich / n8n 之类的应用，看不到就没法用；而这个文件在应用私有数据目录里、
// 权限 0600、沙箱本身又是隔离的。和 docker 把 POSTGRES_PASSWORD 放在环境变量里
// 是同一个量级的暴露。
//
// 这里【没有管理页的密码】：管理页的鉴权完全交给 UGOS 的登录认证能力
// （网关校验 + 注入用户头，见 ugauth.go），应用自己不再实现账号系统。
type Config struct {
	SuperPass string            `json:"super_pass"` // postgres 超级用户密码
	PGPort    int               `json:"pg_port"`
	AllowLAN  bool              `json:"allow_lan"` // 是否允许局域网连接（pg_hba）
	TimeZone  string            `json:"timezone"`
	Roles     map[string]string `json:"roles"` // 我们代建的角色 → 密码，方便用户回查

	// PGBase 是数据库文件的落点（PGDATA 的上一级）。
	//
	// 【为什么要存进配置而不是每次照环境变量重算】：数据目录一旦定下来就不能再漂移。
	// 安装参数首次启动时拿到的是空值（平台先起服务，2~3 秒后才把值写进 .env，
	// 而 .env 是 root:root 0640，应用自己读不到）。要是每次启动都重算，
	// 就会出现"第一次落在 A、重启后落在 B"，用户看到的是数据库突然变空。
	// 所以初始化那一刻定下来、写进配置，此后只认配置；参数后来改了只在管理页提示，
	// 要搬得由人点一次「迁移」。
	PGBase string `json:"pg_base"`
}

func defaultConfig() *Config {
	return &Config{
		SuperPass: randomPassword(20),
		PGPort:    5432,
		AllowLAN:  true,
		TimeZone:  "Asia/Shanghai",
		Roles:     map[string]string{},
	}
}

func main() {
	port := flag.Int("port", 25432, "管理页监听端口")
	flag.Parse()

	log.SetFlags(log.LstdFlags)
	app, err := newApp()
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	log.SetOutput(app.rec) // rec 同时写 stderr 和环形缓冲

	// HTTP 先起来，再去做 PostgreSQL 的初始化。
	// initdb 要好几秒，而绿联会拿这个端口探活 —— 让探活先通过，
	// 页面上再显示"正在初始化"，比让系统以为应用起不来好得多。
	srv := app.startHTTP(*port)

	go func() {
		// 先清上次运行留下的垃圾，再启动。放在这里而不是 newApp 里，
		// 是因为它不该有能力阻止应用起来 —— 清不掉只记一句日志。
		app.housekeeping()
		if err := app.bootPostgres(); err != nil {
			log.Printf("PostgreSQL 启动失败: %v", err)
		}
	}()
	// 上传会话是用户传备份时产生的碎片，跑久了会攒下来。每小时扫一次。
	go app.gcLoop()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	sig := <-stop
	log.Printf("收到信号 %v，开始关闭", sig)

	app.shutdownHTTP(srv)
	app.pg.Stop()
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
		appRoot: appRoot,
		pgRoot:  filepath.Join(appRoot, "pgsql"),
		dataDir: dataDir,
		rec:     newRecorder(4000),
	}
	a.binDir = filepath.Join(a.pgRoot, "lib", "postgresql", pgMajor, "bin")
	a.runtime = filepath.Join(a.pgRoot, "lib", "runtime")
	a.initCwd = filepath.Join(dataDir, "init-cwd")
	a.sockDir = filepath.Join(dataDir, "run")
	a.nssDir = filepath.Join(dataDir, "nss")
	a.uploads = filepath.Join(dataDir, "uploads")
	a.backups = filepath.Join(dataDir, "backups")
	a.logFile = filepath.Join(dataDir, "logs", "postgresql.log")
	a.cfgPath = filepath.Join(dataDir, "config.json")

	for _, d := range []string{a.initCwd, a.sockDir, a.nssDir, a.uploads, a.backups,
		filepath.Dir(a.logFile)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("建目录 %s: %w", d, err)
		}
	}
	if err := a.loadConfig(); err != nil {
		return nil, err
	}
	if err := a.resolvePGBase(); err != nil {
		return nil, err
	}
	a.pg = newSupervisor(a)
	return a, nil
}

// paramDataDir 读安装参数 POSTGRES_DATA_DIR。
//
// 空字符串和字面量 "null" 都算"没填"：平台在用户没选目录时写进 .env 的就是
// 后者（不是空串），照字面用会建出一个名叫 null 的目录。
func paramDataDir() string {
	v := strings.TrimSpace(os.Getenv("POSTGRES_DATA_DIR"))
	if v == "" || v == "null" {
		return ""
	}
	return v
}

// PGBaseFromParam 把安装参数里的共享文件夹换算成真正的数据落点。
//
// 【首次初始化和迁移必须用同一个函数】—— 两条路算出不同的子目录名的话，
// 同一个参数值在"新装"和"迁移"下会指到两个地方，用户完全看不懂。
func PGBaseFromParam(param string) string {
	return filepath.Join(param, pgBaseSubdir)
}

// pgBaseSubdir：数据不直接铺在用户选的共享文件夹根上，放进这个子目录。
// 用户选的往往是一个已经有别的东西的文件夹，往里铺一堆 pg_wal/base/global 很不礼貌，
// 也让"这些文件是谁的"变得说不清。
const pgBaseSubdir = "postgresql17-data"

// resolvePGBase 决定数据落点。只在【尚未初始化】时才真正做选择。
func (a *App) resolvePGBase() error {
	if base := a.config().PGBase; base != "" {
		a.setPGBase(base)
		return nil
	}

	// 配置里没有 → 要么是全新安装，要么是从"还没有这个字段"的老版本升上来的。
	// 老版本的数据一定在 <dataDir>/pgdata，先认它，绝不能因为字段是空的
	// 就把老用户的数据库当成不存在、重新 initdb 一个空的出来。
	legacy := filepath.Join(a.dataDir, pgDataSubdir)
	if _, err := os.Stat(filepath.Join(legacy, "PG_VERSION")); err == nil {
		log.Printf("配置里没有数据落点，但 %s 已有数据库，沿用它（老版本升级上来的）", legacy)
		a.setPGBase(a.dataDir)
		return a.updateConfig(func(c *Config) { c.PGBase = a.dataDir })
	}

	param := paramDataDir()
	chosen := ""
	if param != "" {
		candidate := PGBaseFromParam(param)
		if err := os.MkdirAll(candidate, 0o700); err == nil {
			chosen = candidate
		} else {
			log.Printf("安装参数指定的目录 %s 不可写（%v），改用应用数据目录", param, err)
		}
	}
	if chosen == "" {
		chosen = a.dataDir
	}
	a.setPGBase(chosen)
	return a.updateConfig(func(c *Config) { c.PGBase = chosen })
}

// pgBaseParamChanged 判断"用户后来改了安装参数，但数据还在老地方"。
// 只用来在管理页上提示 —— 搬数据库要停服、要几分钟，必须由人点一次才动。
func (a *App) pgBaseParamChanged() (bool, string) {
	now := paramDataDir()
	if now == "" {
		return false, ""
	}
	target := PGBaseFromParam(now)
	if filepath.Clean(target) == filepath.Clean(a.PGBase()) {
		return false, ""
	}
	return true, target
}

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
	if cfg.Roles == nil {
		cfg.Roles = map[string]string{}
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

// Config 读一份快照。所有 handler 都用这个，避免到处加锁。
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

// housekeeping 清理上次运行留下的、留着只有坏处的东西。必须幂等。
func (a *App) housekeeping() {
	// initdb 的密码文件。正常路径上 runInitdb 会 defer 删掉它，但进程被 SIGKILL
	// （比如系统重启、或者被 OOM 干掉）时 defer 是不跑的，明文密码就留在了盘上。
	pw := filepath.Join(a.dataDir, initPasswordFile)
	if err := os.Remove(pw); err == nil {
		log.Printf("清理了上次残留的 %s（里面是明文密码）", pw)
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

