package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateFailed   State = "failed"
	StateBusy     State = "busy" // 正在做维护（初始化 / 改密码 / 升级）
)

const (
	// InnoDB 关闭要刷脏页，慢的时候真能到几十秒。这个超时给得比一般服务宽得多，
	// 宁可等，也不要 KILL 一个正在写数据的数据库。
	shutdownTimeout = 90 * time.Second
	// 启动后等它开始接受连接的上限。首次建 InnoDB 表空间会慢一些。
	readyTimeout = 120 * time.Second
	// 启动后活过这个时长就算"起稳了"。短于它就退出的算快速失败，用来识别崩溃循环。
	stableAfter = 30 * time.Second
	// 连续快速失败多少次就放弃自动拉起，避免无意义地反复重启一个必然失败的服务。
	maxFastFails = 5
)

// Server 管一个 mariadbd 子进程。
//
// 并发模型（这块出过 bug，写清楚）：
//   - opMu 串行化【所有会改变进程状态的操作】：Start / Stop / Restart /
//     WithStopped / 自动重启。任何时刻只有一个在跑，所以不可能出现
//     "维护操作正在改数据、监管逻辑同时把 mariadbd 拉起来"这种两个进程
//     开同一个数据目录的情况。
//   - mu 只保护下面那几个字段，持有时间都极短，绝不跨越子进程等待。
//   - gen 是启动代次：进程退出的回调拿着自己那一代的号，只有号还对得上
//     才有资格触发自动重启。这样"手动重启"和"崩溃自动拉起"不会打架。
type Server struct {
	p   *Paths
	cfg *Config

	// dataDir 会被「迁移数据目录」在运行期改掉，所以不能裸读裸写。
	// 用一把独立的锁而不是复用下面的 mu：读它的地方（writeConfig 等）
	// 有些是在持有 mu 的路径之外调的，混用会很容易写出自死锁。
	dataDirMu sync.RWMutex
	dataDir   string

	opMu sync.Mutex

	mu        sync.Mutex
	state     State
	lastErr   string
	startedAt time.Time
	version   string
	// 取版本失败后的退避时刻，防止状态轮询把 fork 变成死循环
	versionNextTry time.Time
	desired        bool
	proc           *os.Process
	exited         chan struct{}
	gen            uint64
	fastFails      int
	closed         bool

	// closeCh 在关闭时被 close。退避等待要能被它打断，
	// 否则收到 SIGTERM 时会白白多等十几秒才开始关数据库。
	closeCh   chan struct{}
	closeOnce sync.Once
}

func NewServer(p *Paths, cfg *Config, dataDir string) *Server {
	return &Server{
		p: p, cfg: cfg, dataDir: dataDir,
		state:   StateStopped,
		closeCh: make(chan struct{}),
	}
}

// ---------- 路径 ----------

// DataDir 是数据库数据目录。会被迁移操作改掉，所以一律走这个访问器读。
func (s *Server) DataDir() string {
	s.dataDirMu.RLock()
	defer s.dataDirMu.RUnlock()
	return s.dataDir
}

func (s *Server) setDataDir(d string) {
	s.dataDirMu.Lock()
	defer s.dataDirMu.Unlock()
	s.dataDir = d
}

func (s *Server) socketPath() string { return filepath.Join(s.p.Data, "mariadb.sock") }
func (s *Server) pidPath() string    { return filepath.Join(s.p.Data, "mariadb.pid") }
func (s *Server) cnfPath() string    { return filepath.Join(s.p.Data, "my.cnf") }
func (s *Server) tmpDir() string     { return filepath.Join(s.p.Cache, "tmp") }
func (s *Server) errorLogPath() string {
	return filepath.Join(s.p.Log, "mariadb-error.log")
}

// ---------- 进程构造 ----------

// command 用【显式动态加载器】启动 MariaDB 的可执行文件：
//
//	ld-linux-<arch>.so.N --library-path <包内lib> <程序> <参数...>
//
// 为什么不直接 exec 程序本身：程序的 PT_INTERP 是 /lib/ld-linux-<arch>.so.N，
// 而绿联原生沙箱的文件系统视图极窄（根目录只有 8 项、/usr 不存在），
// 沙箱里 /lib 有什么、会不会随固件升级变，都不由我们决定。显式指定加载器和
// 库搜索路径之后，整个运行时完全自包含，不依赖沙箱里的任何系统库。
func (s *Server) command(bin string, args ...string) *exec.Cmd {
	full := append([]string{"--library-path", s.p.LibDir, bin}, args...)
	cmd := exec.Command(s.p.Loader, full...)
	cmd.Env = append(os.Environ(),
		// --library-path 已经覆盖了主可执行文件的查找，但插件是 dlopen 进来的，
		// 这里再设一遍环境变量保证插件的依赖也能解析到。
		"LD_LIBRARY_PATH="+s.p.LibDir,
		// 沙箱里【没有 /tmp】。不设这个的话，任何走 tmpfile 的路径都会失败，
		// 而且是启动几秒后才炸，看起来像"应用自己停了"。
		"TMPDIR="+s.tmpDir(),
		"HOME="+s.p.Data,
	)
	cmd.Dir = s.p.Data
	return cmd
}

// writeConfig 生成 mariadbd 的配置文件。每次启动都按当前设置重写。
func (s *Server) writeConfig() error {
	d := s.cfg.Snapshot()
	var b strings.Builder
	b.WriteString("# 本文件由 MariaDB 应用的管理壳自动生成，每次启动都会重写。\n")
	b.WriteString("# 手工修改不会保留 —— 端口和监听范围请在应用的管理页面里改。\n\n")
	b.WriteString("[mariadbd]\n")
	fmt.Fprintf(&b, "basedir                 = %s\n", s.p.Base)
	fmt.Fprintf(&b, "datadir                 = %s\n", s.DataDir())
	fmt.Fprintf(&b, "tmpdir                  = %s\n", s.tmpDir())
	fmt.Fprintf(&b, "socket                  = %s\n", s.socketPath())
	fmt.Fprintf(&b, "pid-file                = %s\n", s.pidPath())
	fmt.Fprintf(&b, "log-error               = %s\n", s.errorLogPath())
	fmt.Fprintf(&b, "port                    = %d\n", d.Port)
	fmt.Fprintf(&b, "bind-address            = %s\n", d.BindAddress())
	b.WriteString("\n")
	// 沙箱里没有 /etc/hosts，DNS 反查一定失败；skip-name-resolve 既是【必须的】
	// 也顺带省掉每次建连的一次反查。权限表里因此只能用 IP/通配，不能用主机名。
	b.WriteString("skip-name-resolve\n")
	b.WriteString("skip-host-cache\n")
	// log-warnings=2（默认）会把每次探活造成的连接中断记成告警，把错误日志刷满。
	b.WriteString("log-warnings            = 1\n")
	b.WriteString("\n")
	b.WriteString("character-set-server    = utf8mb4\n")
	b.WriteString("collation-server        = utf8mb4_general_ci\n")
	b.WriteString("\n")
	b.WriteString("innodb_buffer_pool_size = 128M\n")
	b.WriteString("max_connections         = 151\n")
	return os.WriteFile(s.cnfPath(), []byte(b.String()), 0o640)
}

// ---------- 初始化 ----------

// InitState 描述数据目录处于哪一档。
//
// 为什么不能只看「data/mysql 目录非空」（这是 v11.4.12.0003 之前的判据）：
// 初始化分两步——先建系统表、再建 root 账号。第二步失败时目录已经非空了，
// 于是之后每次启动都认为"已初始化"而跳过，数据库永久停在【没有可用账号】的
// 状态，且不报任何错。真机上就这么坏过：登录一律 1045，而 MySQL 的 1045
// 不区分"密码错"和"账号不存在"，从外面完全看不出根因。
//
// 所以改成一个【只在两步都成功后才写】的显式标记。
type InitState int

const (
	InitFresh           InitState = iota // 空目录：要完整初始化
	InitAccountsMissing                  // 系统表在、账号没建完：只需补建账号
	InitComplete                         // 完好
)

// initDoneMarker 只在初始化【全部】成功后写入。
// 故意不复用 mariadb_upgrade_info —— 那个的语义是"上次跑 upgrade 时的版本号"，
// 两件事混用会在版本升级时互相干扰。
const initDoneMarker = ".ugos_init_done"

// classifyDataDir 是纯函数，方便直接对着假目录测。
func classifyDataDir(dataDir string) InitState {
	if _, err := os.Stat(filepath.Join(dataDir, initDoneMarker)); err == nil {
		return InitComplete
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "mysql"))
	if err != nil || len(entries) == 0 {
		return InitFresh
	}
	// 系统表在但没有标记：要么是初始化半途失败，要么是从有这个 bug 的老版本升上来的。
	// 两种都靠补建账号修复，不需要（也绝不该）动已有的数据。
	return InitAccountsMissing
}

func (s *Server) InitState() InitState { return classifyDataDir(s.DataDir()) }

func (s *Server) markInitDone() error {
	return os.WriteFile(filepath.Join(s.DataDir(), initDoneMarker),
		[]byte(time.Now().Format(time.RFC3339)+"\n"), 0o640)
}

var bootstrapSQLFiles = []string{
	"mariadb_system_tables.sql",
	"mariadb_performance_tables.sql",
	"mariadb_system_tables_data.sql",
	"fill_help_tables.sql",
	"maria_add_gis_sp_bootstrap.sql",
	"mariadb_sys_schema.sql",
}

// runBootstrap 用 `mariadbd --bootstrap` 执行一段 SQL：不监听网络、从 stdin 读 SQL。
// 用来建系统表（普通 DDL），这部分一直是好的。
//
// ⚠ 【不要】拿它执行账号类语句。`--bootstrap` 本身就隐含 `--skip-grant-tables`，
// 该模式下 CREATE USER / ALTER USER / GRANT 会被直接拒绝：
//
//	ERROR: 1290  The MariaDB server is running with the --skip-grant-tables
//	             option so it cannot execute this statement
//
// （2026-08-09 真机 MariaDB 11.4.12 实测。本文件早先的注释断言的正好相反，
// 说"不需要 --skip-grant-tables 那一套"，实际 bootstrap 就是它 ——
// 于是每一次全新安装的 root 账号都没建出来。账号语句走 withTempServer。）
//
// 调用方必须持有 opMu 且确保 mariadbd 已停止。
func (s *Server) runBootstrap(sql io.Reader) error {
	if err := os.MkdirAll(s.tmpDir(), 0o750); err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
	}
	cmd := s.command(s.p.Mariadbd,
		// 不读任何外部配置：bootstrap 阶段只认这里显式给的参数，
		// 避免 my.cnf 里的 log-error 把错误吞进日志文件、让我们看不到失败原因。
		"--no-defaults",
		"--bootstrap",
		"--skip-log-error",
		"--basedir="+s.p.Base,
		"--datadir="+s.DataDir(),
		"--tmpdir="+s.tmpDir(),
		"--plugin-dir="+filepath.Join(s.p.LibDir, "plugin"),
		"--lc-messages-dir="+s.p.ShareDir,
		"--character-sets-dir="+filepath.Join(s.p.ShareDir, "charsets"),
		"--log-warnings=0",
		"--enforce-storage-engine=",
		"--max_allowed_packet=8M",
		"--net_buffer_length=16K",
	)
	cmd.Stdin = sql
	var errBuf bytes.Buffer
	cmd.Stdout = io.Discard
	// bootstrap 的报错只有 stderr 这一个出口，必须留着 —— 否则初始化失败时
	// 用户只会看到"应用启动失败"，完全无从下手。
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("mariadbd --bootstrap 失败: %s", tailLines(msg, 20))
	}
	return nil
}

// unix socket 的路径长度上限（sockaddr_un.sun_path）在 Linux 上是 108 字节，
// 留一点余量。超了的表现是 "bind: invalid argument"，报错完全看不出是路径太长。
const maxUnixSocketPath = 100

// tempServerArgs 是维护用临时实例的启动参数。抽出来是为了能直接测 ——
// 少了 --skip-grant-tables 就连不进去（不知道当前密码），
// 少了 --skip-networking 会在维护窗口里把一个【无密码】的实例暴露到端口上，
// 而混进 --bootstrap 又会退回 1290 那个老 bug。三条都得钉住。
func (s *Server) tempServerArgs(sock string) []string {
	return []string{
		"--no-defaults",
		// 这两个一起用：不开网络端口，只有本机这个私有 socket 能连进来。
		// 维护窗口期间外面既连不上、也不需要密码。
		"--skip-grant-tables",
		"--skip-networking",
		"--socket=" + sock,
		"--basedir=" + s.p.Base,
		"--datadir=" + s.DataDir(),
		"--tmpdir=" + s.tmpDir(),
		"--plugin-dir=" + filepath.Join(s.p.LibDir, "plugin"),
		"--lc-messages-dir=" + s.p.ShareDir,
		"--character-sets-dir=" + filepath.Join(s.p.ShareDir, "charsets"),
		"--skip-log-error",
		"--log-warnings=0",
	}
}

// withTempServer 起一个【只走 unix socket、跳过授权表】的临时 mariadbd，
// 把 sqlText 喂给客户端执行，然后收掉它。账号类语句只能走这条路，原因见 runBootstrap。
//
// 关键点：sqlText 的第一句必须是 FLUSH PRIVILEGES —— 它让运行中的实例重新加载
// 并【启用】授权系统，之后 CREATE USER / GRANT 才被允许。见 rootPasswordSQL。
//
// 调用方必须持有 opMu 且确保正式的 mariadbd 已停止。
func (s *Server) withTempServer(sqlText string) error {
	if err := os.MkdirAll(s.tmpDir(), 0o750); err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
	}
	sock := filepath.Join(s.tmpDir(), "init.sock")
	if len(sock) > maxUnixSocketPath {
		return fmt.Errorf("临时 socket 路径过长（%d 字节，上限 %d）：%s",
			len(sock), maxUnixSocketPath, sock)
	}
	_ = os.Remove(sock)

	srv := s.command(s.p.Mariadbd, s.tempServerArgs(sock)...)
	var srvOut bytes.Buffer
	srv.Stdout = &srvOut
	srv.Stderr = &srvOut
	if err := srv.Start(); err != nil {
		return fmt.Errorf("启动临时实例失败: %w", err)
	}

	// 结束时一定要把它收干净：留一个还开着 socket 的实例，
	// 正式实例起来后会两个进程同时写同一个数据目录，后果比失败严重得多。
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	defer func() {
		if srv.Process == nil {
			return
		}
		// ⚠ 不要用 SQL 的 SHUTDOWN 来关它：执行完设密码那段之后 root 已经需要密码，
		// 无密码的客户端连不进去，SHUTDOWN 会直接 1045（真机踩过）。发信号最稳。
		_ = srv.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(shutdownTimeout):
			logf("::warning:: 临时实例没能在 %s 内退出，强制结束", shutdownTimeout)
			_ = srv.Process.Kill()
			<-exited
		}
		_ = os.Remove(sock)
	}()

	// 等它开始接受连接；同时盯着进程是否已经退出，否则起不来时要白等满超时。
	deadline := time.Now().Add(readyTimeout)
	ready := false
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			exited <- err // 放回去给 defer 用
			return fmt.Errorf("临时实例启动即退出（%v）：%s",
				err, tailLines(strings.TrimSpace(srvOut.String()), 20))
		default:
		}
		if _, err := os.Stat(sock); err == nil {
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		return fmt.Errorf("临时实例在 %s 内没有就绪：%s",
			readyTimeout, tailLines(strings.TrimSpace(srvOut.String()), 20))
	}

	cli := s.command(s.p.Client, "--protocol=socket", "--socket="+sock, "--user=root")
	cli.Stdin = strings.NewReader(sqlText)
	out, err := cli.CombinedOutput()
	if err != nil {
		return fmt.Errorf("执行账号 SQL 失败: %s",
			tailLines(strings.TrimSpace(string(out)), 20))
	}
	return nil
}

// Initialize 首次创建数据目录里的系统表，并设定 root 密码。
func (s *Server) Initialize(rootPassword string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	dataDir := s.DataDir()
	logf("正在初始化数据目录 %s（首次启动，需要一点时间）", dataDir)
	s.setState(StateBusy, "")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	var sql bytes.Buffer
	sql.WriteString("create database if not exists mysql;\n")
	sql.WriteString("use mysql;\n")
	// 上游 SQL 里唯一需要外部注入的变量。NULL = 不额外创建 unix_socket 认证账号，
	// root 一律用密码认证（应用跑在随机的 app_xxxx 用户下，socket 认证对不上名字）。
	sql.WriteString("SET @auth_root_socket=NULL;\n")
	for _, name := range bootstrapSQLFiles {
		b, err := os.ReadFile(filepath.Join(s.p.ShareDir, name))
		if err != nil {
			return fmt.Errorf("读取初始化脚本 %s 失败: %w", name, err)
		}
		sql.Write(b)
		sql.WriteString("\n")
	}
	// 第一步：建系统表。纯 DDL，bootstrap 能干。
	//
	// ⚠ 账号语句【不能】拼在这段后面一起丢给 bootstrap —— 那样会撞 1290 使整段中止，
	// 而系统表已经建好了，于是数据目录停在"有表、没账号"的半成品状态。
	// 这正是 0003 之前的写法，导致每一次全新安装都装出一个登录必报 1045 的数据库。
	if err := s.runBootstrap(&sql); err != nil {
		return err
	}

	// 第二步：建 root 账号。必须走临时实例，理由见 withTempServer。
	if err := s.setupAccounts(rootPassword); err != nil {
		return err
	}

	// 记下当前版本，供后续判断是否需要跑 mariadb-upgrade（和上游脚本行为一致）。
	if v := s.Version(); v != "" {
		_ = os.WriteFile(filepath.Join(dataDir, "mariadb_upgrade_info"), []byte(v), 0o640)
	}
	// 两步都成功了才落这个标记。中途失败就让它缺着，下次启动会走补建分支。
	if err := s.markInitDone(); err != nil {
		return fmt.Errorf("写初始化完成标记失败: %w", err)
	}
	s.setState(StateStopped, "")
	logf("数据目录初始化完成")
	return nil
}

// setupAccounts 建/重置 root 账号。幂等，可以反复跑。
// 调用方必须持有 opMu 且确保正式的 mariadbd 已停止。
func (s *Server) setupAccounts(rootPassword string) error {
	sqlText, err := rootPasswordSQL(rootPassword)
	if err != nil {
		return err
	}
	return s.withTempServer(sqlText)
}

// RepairAccounts 修复"系统表在、账号没建完"的数据目录（InitAccountsMissing）。
//
// 这是给存量机器自愈用的：0003 之前的版本装出来的库全都缺 root 账号，
// 用户表现是无论填什么密码都 1045。这里只补账号，【绝不碰任何用户数据】。
func (s *Server) RepairAccounts(rootPassword string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	logf("检测到数据目录缺少 root 账号（初始化未完成），正在补建 —— 不会动任何已有数据")
	s.setState(StateBusy, "")
	if err := s.setupAccounts(rootPassword); err != nil {
		return err
	}
	if err := s.markInitDone(); err != nil {
		return fmt.Errorf("写初始化完成标记失败: %w", err)
	}
	logf("root 账号已补建，密码就是管理页里显示的那个")
	s.setState(StateStopped, "")
	return nil
}

// rootPasswordSQL 生成设定 root 密码的语句。
//
// 上游的 mariadb_system_tables_data.sql 建出来的 root@localhost 是
// "unix_socket 或一个无效密码"，也就是【默认根本没法用密码登录】。
// 这里统一改成密码认证，并补上远程可用的 root@'%'。
// 远程能不能真的连上由 bind-address 决定（管理页里的局域网开关），不是靠账号限制。
func rootPasswordSQL(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	q := quoteSQLString(password)
	var b strings.Builder
	// ⚠ 这一句必须在最前面，而且不能删。
	//
	// 承载它的临时实例是用 --skip-grant-tables 起的（不这样起就没法在不知道当前
	// 密码的情况下连进去）。那个状态下授权系统是关着的，CREATE USER / GRANT 会被
	// 拒绝并报 1290。FLUSH PRIVILEGES 会让运行中的实例重新加载权限表并【启用】
	// 授权系统，之后下面这些语句才被允许。
	//
	// 末尾还有一次 FLUSH，那次的作用不同：让刚写进去的账号立即生效。
	b.WriteString("FLUSH PRIVILEGES;\n")
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "%"} {
		h := quoteSQLString(host)
		fmt.Fprintf(&b, "CREATE USER IF NOT EXISTS 'root'@%s IDENTIFIED BY %s;\n", h, q)
		fmt.Fprintf(&b, "ALTER USER 'root'@%s IDENTIFIED BY %s;\n", h, q)
		fmt.Fprintf(&b, "GRANT ALL PRIVILEGES ON *.* TO 'root'@%s WITH GRANT OPTION;\n", h)
	}
	// 匿名账号在部分构建里会被建出来，留着等于给局域网开了一道无密码的口子。
	b.WriteString("DELETE FROM mysql.global_priv WHERE User='';\n")
	b.WriteString("FLUSH PRIVILEGES;\n")
	return b.String(), nil
}

// isAuthFailure 判断一段客户端输出是不是"登录被拒"。
// 按错误号 1045 匹配而不是匹配英文文案 —— 文案会随语言包和版本变。
func isAuthFailure(out string) bool {
	return strings.Contains(out, "1045") || strings.Contains(out, "Access denied")
}

// ValidatePassword 挡住会破坏 SQL 语句或让人自锁的密码。
func ValidatePassword(p string) error {
	if len(p) < 8 {
		return fmt.Errorf("密码至少 8 个字符")
	}
	if len(p) > 128 {
		return fmt.Errorf("密码最长 128 个字符")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("密码不能包含控制字符或换行")
		}
	}
	return nil
}

// quoteSQLString 把字符串转成 SQL 字面量。
// 密码是用户输入的，这里【必须】转义 —— 一个单引号就能让整条语句语义错乱。
func quoteSQLString(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`'`, `\'`,
		"\x00", `\0`,
	)
	return "'" + r.Replace(s) + "'"
}

// ---------- 版本 ----------

var versionRe = regexp.MustCompile(`Ver\s+(\S+)`)

// Version 返回 mariadbd 自报的版本（如 11.4.12-MariaDB）。结果缓存。
func (s *Server) Version() string {
	s.mu.Lock()
	v := s.version
	// 取版本要 fork 一个子进程。管理页面每 5 秒轮询一次状态，而状态接口在
	// 版本为空时会来调它 —— 包坏掉导致这里一直失败的话，就变成每 5 秒
	// fork 一次、永远不停。所以失败之后也要退避。
	retryOK := s.versionNextTry.IsZero() || time.Now().After(s.versionNextTry)
	s.mu.Unlock()
	if v != "" || !retryOK {
		return v
	}

	out, err := s.command(s.p.Mariadbd, "--no-defaults", "--version").Output()
	if err != nil {
		s.mu.Lock()
		s.versionNextTry = time.Now().Add(60 * time.Second)
		s.mu.Unlock()
		return ""
	}
	m := versionRe.FindSubmatch(out)
	if m == nil {
		return ""
	}
	v = string(m[1])
	s.mu.Lock()
	s.version = v
	s.mu.Unlock()
	return v
}

// MaybeUpgrade 在包内 MariaDB 版本比数据目录记录的版本新时跑一次 mariadb-upgrade。
// 应用自动更新到新的小版本后，不跑这个可能出现系统表结构对不上的问题。
func (s *Server) MaybeUpgrade(rootPassword string) {
	cur := s.Version()
	if cur == "" {
		return
	}
	infoPath := filepath.Join(s.DataDir(), "mariadb_upgrade_info")
	prev, _ := os.ReadFile(infoPath)
	if strings.TrimSpace(string(prev)) == cur {
		return
	}
	logf("检测到版本变化（%q → %q），执行 mariadb-upgrade", strings.TrimSpace(string(prev)), cur)
	cmd := s.command(s.p.Upgrade,
		"--socket="+s.socketPath(),
		"--user=root",
		"--password="+rootPassword,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// 认证失败要单独拎出来：它不是"升级没成功"，而是【root 账号是坏的】——
		// 数据库是活着的，但谁也登不进去。这条信号极准，以前只打 warning 埋在日志里，
		// 用户只能看到 phpMyAdmin/Navicat 报一句 1045，根本猜不到发生了什么。
		// 所以把它升级成故障状态，管理页上能直接看到。
		if isAuthFailure(string(out)) {
			msg := "root 账号无法登录（1045）—— 数据目录里的账号很可能没建成功。" +
				"请在本页点「修改 root 密码」重置一次；它不需要知道当前密码。"
			logf("::error:: %s 原始输出：%s", msg, tailLines(string(out), 6))
			s.setState(StateFailed, msg)
			return
		}
		// 其它原因的升级失败不该拦住启动：服务通常还是能用的，把原因喊出来让用户决定。
		logf("::warning:: mariadb-upgrade 未成功（%v）：%s", err, tailLines(string(out), 10))
		return
	}
	_ = os.WriteFile(infoPath, []byte(cur), 0o640)
	logf("mariadb-upgrade 完成")
}

// ---------- 状态 ----------

func (s *Server) setState(st State, errMsg string) {
	s.mu.Lock()
	s.state = st
	if errMsg != "" {
		s.lastErr = errMsg
	} else if st == StateRunning {
		s.lastErr = ""
	}
	s.mu.Unlock()
}

type Status struct {
	State     State  `json:"state"`
	LastError string `json:"last_error,omitempty"`
	Version   string `json:"version,omitempty"`
	StartedAt int64  `json:"started_at,omitempty"`
}

func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{State: s.state, LastError: s.lastErr, Version: s.version}
	if !s.startedAt.IsZero() && s.state == StateRunning {
		st.StartedAt = s.startedAt.Unix()
	}
	return st
}

func (s *Server) Desired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desired
}

func (s *Server) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc != nil
}

// ---------- 进程启停（都要在持有 opMu 时调用）----------

// startLocked 起进程并等它就绪。
func (s *Server) startLocked() error {
	if s.isRunning() {
		return nil
	}
	if err := os.MkdirAll(s.tmpDir(), 0o750); err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
	}
	if err := s.writeConfig(); err != nil {
		return fmt.Errorf("写 my.cnf 失败: %w", err)
	}

	cmd := s.command(s.p.Mariadbd, "--defaults-file="+s.cnfPath())
	// mariadbd 自己会把日志写进 log-error 指定的文件；这里再把 stdout/stderr
	// 接到我们的日志，捕获【日志系统初始化之前】就失败的那类错误（比如缺库）。
	cmd.Stdout = logWriter{"mariadbd"}
	cmd.Stderr = logWriter{"mariadbd"}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 mariadbd 失败: %w", err)
	}

	exited := make(chan struct{})
	s.mu.Lock()
	s.gen++
	gen := s.gen
	s.proc = cmd.Process
	s.exited = exited
	s.state = StateStarting
	s.startedAt = time.Now()
	s.mu.Unlock()

	go func() {
		waitErr := cmd.Wait()
		s.mu.Lock()
		s.proc = nil
		s.mu.Unlock()
		close(exited)
		// 交给独立协程处理自动重启：它要抢 opMu，而这里可能正被
		// stopLocked 等着（stopLocked 持有 opMu），不能在这条路径上阻塞。
		go s.onExit(gen, waitErr)
	}()

	if err := s.waitReady(exited, readyTimeout); err != nil {
		s.stopLocked()
		return err
	}
	s.setState(StateRunning, "")
	d := s.cfg.Snapshot()
	logf("MariaDB 已就绪：%s，端口 %d，监听 %s", s.Version(), d.Port, d.BindAddress())
	return nil
}

// waitReady 等 mariadbd 开始接受连接。
// 判据是 unix socket 能连上 —— 比看日志或看 pid 文件都可靠。
func (s *Server) waitReady(exited <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("mariadbd 在就绪之前就退出了，详见错误日志 %s", s.errorLogPath())
		case <-s.closeCh:
			// 必须能被关闭信号打断：这个循环最长等 120 秒，而它是在持有 opMu
			// 的情况下跑的 —— 不响应的话，收到 SIGTERM 后 Close() 要干等到它结束。
			return fmt.Errorf("等待就绪时收到关闭信号")
		default:
		}
		if c, err := net.DialTimeout("unix", s.socketPath(), 2*time.Second); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("等待 mariadbd 就绪超时（%s）", timeout)
}

// stopLocked 优雅停止 mariadbd：先 SIGTERM，超时才 SIGKILL。
// 数据库的关闭要刷脏页，绝不能一上来就 KILL。
func (s *Server) stopLocked() {
	s.mu.Lock()
	proc, exited := s.proc, s.exited
	// 代次前进：让还没跑的 onExit 认出自己已经过时，不去触发自动重启。
	s.gen++
	s.mu.Unlock()
	if proc == nil || exited == nil {
		s.setState(StateStopped, "")
		return
	}
	logf("正在停止 mariadbd（最多等 %s，数据库关闭要刷盘）", shutdownTimeout)
	_ = proc.Signal(syscall.SIGTERM)
	select {
	case <-exited:
		logf("mariadbd 已停止")
	case <-time.After(shutdownTimeout):
		logf("::warning:: mariadbd %s 内没有退出，强制结束", shutdownTimeout)
		_ = proc.Kill()
		<-exited
	}
	s.setState(StateStopped, "")
}

// onExit 是进程意外退出后的自动拉起逻辑。
//
// gen 是它启动时那一代的号。手动 Stop/Restart 会让 gen 前进，
// 于是这里一看号对不上就知道"这次退出是别人有意为之"，直接不管。
func (s *Server) onExit(gen uint64, waitErr error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	s.mu.Lock()
	stale := s.closed || !s.desired || s.gen != gen
	startedAt := s.startedAt
	s.mu.Unlock()
	if stale {
		return
	}

	reason := "正常退出"
	if waitErr != nil {
		reason = waitErr.Error()
	}
	s.setState(StateFailed, fmt.Sprintf("mariadbd 意外退出（%s），详见错误日志 %s",
		reason, s.errorLogPath()))

	if time.Since(startedAt) >= stableAfter {
		// 跑稳过再退出的，当成偶发故障：立刻重来并把快速失败计数清零
		s.mu.Lock()
		s.fastFails = 0
		s.mu.Unlock()
		logf("::warning:: mariadbd 意外退出，正在重新拉起")
	} else {
		s.mu.Lock()
		s.fastFails++
		fails := s.fastFails
		s.mu.Unlock()
		logf("::warning:: mariadbd 启动后 %s 内就退出了（第 %d 次）",
			time.Since(startedAt).Round(time.Second), fails)
		if fails >= maxFastFails {
			msg := fmt.Sprintf("mariadbd 连续 %d 次启动后很快退出，已停止自动重试。"+
				"请查看错误日志 %s，修好后在管理页面点启动。", maxFastFails, s.errorLogPath())
			s.mu.Lock()
			s.desired = false
			s.mu.Unlock()
			s.setState(StateFailed, msg)
			logf("::error:: %s", msg)
			return
		}
		select {
		case <-time.After(time.Duration(fails) * 3 * time.Second):
		case <-s.closeCh:
			return
		}
	}

	s.mu.Lock()
	giveUp := s.closed || !s.desired
	s.mu.Unlock()
	if giveUp {
		return
	}
	if err := s.startLocked(); err != nil {
		s.setState(StateFailed, err.Error())
		logf("::error:: 自动重启失败: %v", err)
	}
}

// ---------- 对外操作 ----------

func (s *Server) Start() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	s.desired = true
	s.fastFails = 0
	s.mu.Unlock()
	if err := s.startLocked(); err != nil {
		s.setState(StateFailed, err.Error())
		return err
	}
	return nil
}

func (s *Server) Stop() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	s.desired = false
	s.mu.Unlock()
	s.stopLocked()
	return nil
}

// Restart 必须是【一个】操作。
// 早前写成 Stop() 紧接 Start()，两次调用之间监管逻辑看到的期望状态已经是"运行"，
// 于是重启被整个跳过 —— 表现是改了端口/监听范围却完全不生效。
func (s *Server) Restart() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.stopLocked()
	s.mu.Lock()
	s.desired = true
	s.fastFails = 0
	s.mu.Unlock()
	if err := s.startLocked(); err != nil {
		s.setState(StateFailed, err.Error())
		return err
	}
	return nil
}

// WithStopped 把服务停下来、执行 fn、再按原来的期望状态恢复。
// 改密码这类必须独占数据目录的操作走这里。
func (s *Server) WithStopped(fn func() error) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	s.mu.Lock()
	wasDesired := s.desired
	s.mu.Unlock()

	s.stopLocked()
	s.setState(StateBusy, "")
	err := fn()

	// fn 里可以把 desired 改掉，用来表达"这次操作之后请务必把服务拉起来"。
	// 迁移数据目录就用到了：默认语义是"操作前停着的、操作后还停着"，
	// 那对改密码是对的，但用户点「迁移」的意思就是"搬完继续用"——
	// 真机上出现过迁移成功却不自动启动、还得再点一次「启动」的情况。
	s.mu.Lock()
	if s.desired {
		wasDesired = true
	}
	s.mu.Unlock()

	if wasDesired {
		if startErr := s.startLocked(); startErr != nil {
			s.setState(StateFailed, startErr.Error())
			if err == nil {
				err = startErr
			}
		}
	} else {
		s.setState(StateStopped, "")
	}
	return err
}

// Close 关闭服务，不再自动拉起。
func (s *Server) Close() {
	// 先放出关闭信号再抢 opMu：正在退避等待的 onExit 才能立刻醒过来放手。
	s.closeOnce.Do(func() { close(s.closeCh) })
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	s.closed = true
	s.desired = false
	s.mu.Unlock()
	s.stopLocked()
}

// ChangeRootPassword 改 root 密码。
// 走 bootstrap 模式，所以【即使当前密码已经丢了也能改】，这就是"重置"功能。
func (s *Server) ChangeRootPassword(newPassword string) error {
	// 先校验一次，别等停了服务才发现密码不合法。
	if _, err := rootPasswordSQL(newPassword); err != nil {
		return err
	}
	// 走临时实例而不是 bootstrap：账号语句在 bootstrap 里会被 1290 拒绝（见 runBootstrap）。
	// 这条路同样不需要知道当前密码，所以"忘记密码后重置"依然成立。
	return s.WithStopped(func() error {
		return s.setupAccounts(newPassword)
	})
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
