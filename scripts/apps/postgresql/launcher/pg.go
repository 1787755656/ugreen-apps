package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// pgCommand 构造一条运行自带 PostgreSQL 可执行文件的命令。
//
// 三件事必须一起做对，少一件在真机上都是"启动即退、日志里什么都没有"：
//
//  1. 【用自带的 loader】沙箱里没有 /usr/lib，系统 loader 在不在、在哪都不由我们决定。
//     所以显式地跑 ld-linux --library-path <我们的runtime> <真身>.bin。
//     注意这里走 .bin 而不是同名的 sh 包装脚本 —— 少依赖一次 /bin/sh。
//     （包装脚本仍然要有：initdb 会自己 popen 一条 "postgres --single"，那条命令行
//     我们插不上手，只能靠包装脚本让它也走自带 loader。）
//
//  2. 【cwd 必须显式给】postgres 和 initdb 里的时区目录被我们改成了相对路径
//     ../zoneinfo，所以 cwd 的上一级必须是 <dataDir>。
//
//  3. 【LD_PRELOAD nss_wrapper】沙箱的 /etc 只有 4 项、没有 passwd，
//     而 initdb 和 postgres --single 都要 getpwuid(geteuid()) 问自己叫什么名字，
//     查不到就直接退出。
func (a *App) pgCommand(ctx context.Context, name, cwd string, args ...string) *exec.Cmd {
	loader := filepath.Join(a.runtime, loaderName())
	real := filepath.Join(a.binDir, name+".bin")
	full := append([]string{"--library-path", a.runtime, real}, args...)

	cmd := exec.CommandContext(ctx, loader, full...)
	cmd.Dir = cwd
	cmd.Env = []string{
		"LD_PRELOAD=" + filepath.Join(a.runtime, "libnss_wrapper.so"),
		"NSS_WRAPPER_PASSWD=" + filepath.Join(a.nssDir, "passwd"),
		"NSS_WRAPPER_GROUP=" + filepath.Join(a.nssDir, "group"),
		"LD_LIBRARY_PATH=" + a.runtime,
		// initdb 的 popen 要 /bin/sh；沙箱里这些目录只有声明了
		// SYSTEM.EXEC_SYSTEM_COMMAND 才会被挂进来。
		"PATH=/bin:/usr/bin",
		"HOME=" + a.dataDir,
		// 沙箱里【没有 /tmp】（或者是私有的、可能被清），一律指到我们自己的目录。
		"TMPDIR=" + filepath.Join(a.dataDir, "tmp"),
		"PGCLIENTENCODING=UTF8",
	}
	return cmd
}

// ensureZoneinfoLink 在 dir 下建/修一个指向包内 tzdata 的软链。
//
// 每次启动都重建，且必须幂等：应用升级或迁移安装目录之后 appRoot 会变，
// 老软链会指向一个已经不存在的路径，而失败症状是"postgres 启动即退、
// 日志里什么都没有"（它在读 postgresql.conf 之前就要 pg_tzset("GMT")）。
func (a *App) ensureZoneinfoLink(dir string) error {
	link := filepath.Join(dir, "zoneinfo")
	target := filepath.Join(a.pgRoot, "zoneinfo")
	if cur, err := os.Readlink(link); err != nil || cur != target {
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("建时区软链 %s → %s: %w", link, target, err)
		}
	}
	if _, err := os.Stat(filepath.Join(link, "UTC")); err != nil {
		return fmt.Errorf("时区数据不可用（%s）：没有它 PostgreSQL 连配置文件都读不到: %w",
			filepath.Join(link, "UTC"), err)
	}
	return nil
}

// ensureRuntimeBits 补上沙箱里缺的东西。每次启动都跑，且必须是幂等的 ——
// 应用升级或迁移安装目录之后 appRoot 会变，软链要跟着重建。
func (a *App) ensureRuntimeBits() error {
	if err := os.MkdirAll(filepath.Join(a.dataDir, "tmp"), 0o700); err != nil {
		return err
	}

	// 1) 时区目录软链。要建【两处】，因为两个进程的 cwd 不在同一个父目录下：
	//
	//	initdb    cwd = <dataDir>/init-cwd  → ../zoneinfo = <dataDir>/zoneinfo
	//	postgres  cwd = <pgBase>/pgdata     → ../zoneinfo = <pgBase>/zoneinfo
	//
	// 数据没搬走时这两个是同一个路径，搬走之后就不是了 —— 迁移功能最容易在这里
	// 出事：数据复制得好好的，新位置起不来，因为那边没有 zoneinfo。
	base := a.PGBase()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return fmt.Errorf("建数据落点 %s: %w", base, err)
	}
	if err := a.ensureZoneinfoLink(a.dataDir); err != nil {
		return err
	}
	if base != a.dataDir {
		if err := a.ensureZoneinfoLink(base); err != nil {
			return err
		}
	}

	// 2) nss_wrapper 的 passwd/group。用真实 uid/gid，名字固定成 postgres。
	uid, gid := os.Getuid(), os.Getgid()
	passwd := fmt.Sprintf("%s:x:%d:%d:PostgreSQL:%s:/bin/sh\n", pgUser, uid, gid, a.dataDir)
	group := fmt.Sprintf("%s:x:%d:\n", pgUser, gid)
	if err := os.WriteFile(filepath.Join(a.nssDir, "passwd"), []byte(passwd), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(a.nssDir, "group"), []byte(group), 0o600); err != nil {
		return err
	}
	return nil
}

// ---- 初始化状态 ----------------------------------------------------------
//
// 【只看 PG_VERSION 在不在是不够的】。initdb 很早就会写出 PG_VERSION，之后还要跑
// bootstrap、建 template1/information_schema、设密码、fsync，前后几十秒。
// 这中间进程被杀掉（系统重启、用户在管理页里点了停止、OOM），留下的目录就是
// "有 PG_VERSION 但根本不能用"。照旧逻辑判定它"已初始化"，之后每次启动都是
// postgres 起来就崩，而页面上只会说"意外退出"，用户永远不知道该怎么办。
//
// 所以照 MariaDB 那次的教训办：【完成标记只在全部成功之后才写】，
// 判定状态时以标记为准。

// initDoneMarker 放在 PGDATA 里面，跟着数据一起迁移。
// PostgreSQL 不关心 PGDATA 里多出来的普通文件。
const initDoneMarker = ".ugos_init_done"

// initPasswordFile 是喂给 initdb --pwfile 的临时文件，放在 dataDir 下。
// 用文件不用命令行参数：命令行会出现在 /proc/<pid>/cmdline 里。
const initPasswordFile = ".initpw"

type InitState int

const (
	// InitFresh：干净的、还没初始化过 —— 可以直接 initdb。
	InitFresh InitState = iota
	// InitReady：初始化完成过，可以启动。
	InitReady
	// InitBroken：有东西但不完整。【绝不能在这上面启动，也绝不能自动清掉】。
	InitBroken
)

// classifyPGData 判断 PGDATA 的状态。抽成纯函数是为了能对着临时目录测 ——
// 真机上造一个"initdb 到一半被杀"的现场很难。
func classifyPGData(pgData string) (InitState, string) {
	entries, err := os.ReadDir(pgData)
	if os.IsNotExist(err) {
		return InitFresh, "目录还不存在"
	}
	if err != nil {
		return InitBroken, fmt.Sprintf("读不了数据目录：%v", err)
	}
	if len(entries) == 0 {
		return InitFresh, "目录是空的"
	}

	hasVersion := fileExists(filepath.Join(pgData, "PG_VERSION"))
	hasMarker := fileExists(filepath.Join(pgData, initDoneMarker))

	if hasVersion && hasMarker {
		return InitReady, "已初始化"
	}

	// 没有标记，但可能是【本应用的老版本】装的 —— 那时候还不写标记。
	// 判据不能只用 PG_VERSION（半截的 initdb 也有），要找一个"数据库确实被本应用
	// 成功启动过"的痕迹：writeConfFiles 往 postgresql.conf 末尾追加的那两行 include。
	// 它只在 initdb 成功返回【之后】才写，半截的 initdb 产物里不会有。
	if hasVersion && confHasUgosInclude(filepath.Join(pgData, "postgresql.conf")) {
		return InitReady, "已初始化（老版本装的，没有完成标记）"
	}

	if hasVersion {
		return InitBroken, "有 PG_VERSION 但没有初始化完成的痕迹，" +
			"多半是上次 initdb 跑到一半被中断了"
	}
	return InitBroken, fmt.Sprintf("目录里有 %d 个条目但不是一个 PostgreSQL 数据目录", len(entries))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func confHasUgosInclude(path string) bool {
	buf, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(buf, []byte("postgresql.ugos.conf"))
}

// markInitDone 写完成标记。【只在 initdb 完整成功之后调】。
func markInitDone(pgData string) error {
	return os.WriteFile(filepath.Join(pgData, initDoneMarker),
		[]byte("由绿联 PostgreSQL 管理壳写入，表示这个数据目录初始化完整。\n"+
			"删掉它会让应用把这里当成损坏的目录而拒绝启动。\n"), 0o600)
}

// initState 是 classifyPGData 的实例方法版本，顺带把"老版本装的"补上标记。
func (a *App) initState() (InitState, string) {
	pgData := a.PGData()
	st, why := classifyPGData(pgData)
	if st == InitReady && !fileExists(filepath.Join(pgData, initDoneMarker)) {
		if err := markInitDone(pgData); err != nil {
			log.Printf("补写初始化完成标记失败（不影响使用）: %v", err)
		}
	}
	return st, why
}

// initialized 保留给"只想知道能不能连"的调用方。
func (a *App) initialized() bool {
	st, _ := classifyPGData(a.PGData())
	return st == InitReady
}

// localeAttempt 是一组 initdb 的编码/排序规则参数。
//
// 为什么要一条回退链而不是钉死一组：沙箱里【没有 locale-archive】，
// glibc 只有编译进去的 C 和 C.UTF-8 可用；而 --builtin-locale 是 PostgreSQL 17
// 才有的参数。任何一项猜错，initdb 都会失败并且报错信息很难和"环境缺东西"联系起来。
// 所以按"最理想 → 最保守"依次真跑一遍，用第一个成功的。
type localeAttempt struct {
	desc string
	args []string
}

var localeAttempts = []localeAttempt{
	{
		// PG 17 的内置 provider：排序和大小写规则由 PostgreSQL 自己实现，
		// 完全不依赖 glibc 的 locale 数据，是沙箱里最稳的一档。
		desc: "builtin C.UTF-8",
		args: []string{"--encoding=UTF8", "--locale-provider=builtin",
			"--builtin-locale=C.UTF-8"},
	},
	{
		// glibc 2.35+ 把 C.UTF-8 编进了 libc 本体，不需要 locale-archive。
		desc: "libc C.UTF-8",
		args: []string{"--encoding=UTF8", "--locale-provider=libc", "--locale=C.UTF-8"},
	},
	{
		// 最后的兜底。UTF8 + C locale 会被 initdb 判为编码不匹配而拒绝，
		// 所以这一档必须连编码一起退到 SQL_ASCII。
		desc: "libc C / SQL_ASCII",
		args: []string{"--encoding=SQL_ASCII", "--locale-provider=libc", "--locale=C"},
	},
}

func (a *App) runInitdb() error {
	cfg := a.config()
	pgData := a.PGData()

	// 密码走文件不走命令行：命令行参数会出现在进程的 cmdline 里。
	pwFile := filepath.Join(a.dataDir, initPasswordFile)
	if err := os.WriteFile(pwFile, []byte(cfg.SuperPass+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(pwFile)

	var lastOut string
	for _, at := range localeAttempts {
		args := append([]string{
			"--pgdata=" + pgData,
			"--username=" + pgUser,
			"--pwfile=" + pwFile,
			// 本地 unix socket 用 trust：socket 在应用私有数据目录里（0700），
			// 只有本应用的用户进得来，再加一道密码没有实际收益，
			// 反而会让 launcher 自己的管理操作变复杂。
			"--auth-local=trust",
			"--auth-host=scram-sha-256",
		}, at.args...)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		cmd := a.pgCommand(ctx, "initdb", a.initCwd, args...)
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			// ⚠ 完成标记【必须】是最后一步，而且只在这条成功路径上写。
			// 写不出来就当整个初始化失败 —— 宁可下次重来，也不能留下一个
			// "看起来能用、其实状态不明"的数据目录。
			if err := markInitDone(pgData); err != nil {
				return fmt.Errorf("initdb 成功但写不出完成标记（%w）——"+
					"数据目录 %s 所在的位置可能不可写，请换一个", err, pgData)
			}
			log.Printf("initdb 成功（%s），数据目录 %s", at.desc, pgData)
			return nil
		}
		lastOut = string(out)
		log.Printf("initdb 用 %s 失败：%v\n%s", at.desc, err, tail(lastOut, 800))
		// initdb 失败时会自己清掉它建的目录，但不保证一定干净 ——
		// 残留一点东西下一档就会报"目录非空"，白白浪费一次机会。
		_ = os.RemoveAll(pgData)
	}
	return fmt.Errorf("initdb 三种 locale 组合全部失败，最后一次输出：\n%s", tail(lastOut, 2000))
}

// writeConfFiles 生成由管理壳托管的配置。每次启动重写。
//
// 分成三个文件是为了让"我们管的"和"用户改的"互不打架：
//
//	postgresql.conf         initdb 生成的原始文件，我们只在末尾追加两行 include
//	postgresql.ugos.conf    本函数生成，每次启动覆盖 —— 别手改
//	postgresql.custom.conf  用户在管理页里改，最后加载所以优先级最高
func (a *App) writeConfFiles() error {
	cfg := a.config()

	listen := "'*'"
	if !cfg.AllowLAN {
		// 【不能写 'localhost'】：沙箱里没有 /etc/hosts，解析不了这个名字。
		// 同机的其它绿联应用走 127.0.0.1 照样连得到。
		listen = "'127.0.0.1,::1'"
	}
	ugos := fmt.Sprintf(`# 本文件由 PostgreSQL 管理壳生成，【每次启动都会被覆盖】。
# 要自定义请用同目录下的 postgresql.custom.conf（在本文件之后加载，优先级更高），
# 或者直接在应用的管理页里改。
listen_addresses = %s
port = %d
unix_socket_directories = '%s'
# 没有打包 llvmjit 模块（它会把 100MB 的 libLLVM 拖进来），显式关掉免得日志报找不到。
jit = off
# 沙箱里 /proc 只有 self，探测不到大页，显式关掉。
huge_pages = off
# 日志由管理壳接管：直接收 stderr，好在管理页里显示。
logging_collector = off
log_destination = 'stderr'
log_line_prefix = '%%m [%%p] %%q%%u@%%d '
timezone = '%s'
log_timezone = '%s'
password_encryption = scram-sha-256
`, listen, cfg.PGPort, a.sockDir, cfg.TimeZone, cfg.TimeZone)

	if err := os.WriteFile(filepath.Join(a.PGData(), "postgresql.ugos.conf"),
		[]byte(ugos), 0o600); err != nil {
		return err
	}
	custom := filepath.Join(a.PGData(), "postgresql.custom.conf")
	if _, err := os.Stat(custom); os.IsNotExist(err) {
		_ = os.WriteFile(custom, []byte("# 在这里写你自己的 PostgreSQL 配置，"+
			"每行一条，例如：\n# shared_buffers = 512MB\n"), 0o600)
	}

	// 把两行 include 追加到 postgresql.conf（只加一次）。
	mainConf := filepath.Join(a.PGData(), "postgresql.conf")
	buf, err := os.ReadFile(mainConf)
	if err != nil {
		return err
	}
	if !bytes.Contains(buf, []byte("postgresql.ugos.conf")) {
		buf = append(buf, []byte("\n# --- 以下由绿联 PostgreSQL 管理壳追加 ---\n"+
			"include_if_exists = 'postgresql.ugos.conf'\n"+
			"include_if_exists = 'postgresql.custom.conf'\n")...)
		if err := os.WriteFile(mainConf, buf, 0o600); err != nil {
			return err
		}
	}

	// pg_hba.conf 也由我们托管。
	hba := fmt.Sprintf(`# 本文件由 PostgreSQL 管理壳生成，【每次启动都会被覆盖】。
# 要改访问范围请到应用的管理页里改"允许局域网连接"。
local   all   all                  trust
host    all   all   127.0.0.1/32   scram-sha-256
host    all   all   ::1/128        scram-sha-256
`)
	if cfg.AllowLAN {
		hba += "host    all   all   0.0.0.0/0      scram-sha-256\n" +
			"host    all   all   ::/0           scram-sha-256\n"
	}
	return os.WriteFile(filepath.Join(a.PGData(), "pg_hba.conf"), []byte(hba), 0o600)
}

// bootPostgres 是启动时的完整流程。HTTP 服务已经在跑了，所以这里慢一点没关系。
//
// 【任何一条失败路径都必须把原因写进 phase】—— 用户没有 SSH，管理页是他唯一的
// 信息来源。只在日志里 log 一句、页面上还显示"尚未启动"，等于什么都没说。
func (a *App) bootPostgres() (err error) {
	defer func() {
		if err != nil {
			a.pg.setPhase("启动失败：" + err.Error())
			a.pg.setLastErr(err.Error())
		}
	}()

	if err := a.ensureRuntimeBits(); err != nil {
		return fmt.Errorf("准备运行环境: %w", err)
	}
	switch st, why := a.initState(); st {
	case InitFresh:
		a.pg.setPhase("正在初始化数据库，首次启动需要几十秒…")
		log.Printf("首次运行，开始 initdb（数据目录 %s）", a.PGData())
		if err := a.runInitdb(); err != nil {
			return fmt.Errorf("初始化数据库: %w", err)
		}
	case InitBroken:
		// 【绝不在这上面启动，也绝不自动清掉】。自动清是最诱人也最危险的选择：
		// 判据万一错了，用户的数据库就没了。留给人在管理页上点一次
		// 「清空并重新初始化」—— 那条路径也只是改名，不删。
		return fmt.Errorf("数据目录 %s 状态异常（%s）。"+
			"应用不会在这样的目录上启动，也不会自动清理它。"+
			"确认里面没有要保留的数据后，可以在管理页上点「清空并重新初始化」"+
			"（旧目录会改名保留，不会被删除）", a.PGData(), why)
	}
	if err := a.writeConfFiles(); err != nil {
		return fmt.Errorf("写配置文件: %w", err)
	}
	return a.pg.Start()
}

// ReinitPGData 把当前这个（损坏的）数据目录改名让开，然后重新初始化。
//
// 【只改名不删除】。判定"损坏"靠的是启发式，万一判错了，删掉就再也回不来了；
// 改名的代价只是占着盘，用户确认没问题之后自己删。
func (a *App) ReinitPGData() (string, error) {
	if a.pg.Status().Running {
		return "", fmt.Errorf("数据库还在运行，先停止再重新初始化")
	}
	if a.mig.Snapshot().Running {
		return "", fmt.Errorf("正在迁移数据目录，请等它结束")
	}
	pgData := a.PGData()
	st, why := a.initState()
	if st == InitReady {
		return "", fmt.Errorf("当前数据目录是完好的（%s），不需要重新初始化。"+
			"要清空数据请直接删库，或者卸载重装应用", why)
	}

	retired := ""
	if st == InitBroken {
		retired = pgData + ".broken-" + time.Now().Format("20060102-150405")
		if err := os.Rename(pgData, retired); err != nil {
			return "", fmt.Errorf("把旧目录改名到 %s 失败: %w", retired, err)
		}
		log.Printf("已把状态异常的数据目录改名为 %s（没有删除）", retired)
	}
	if err := a.runInitdb(); err != nil {
		return retired, fmt.Errorf("重新初始化失败: %w", err)
	}
	if err := a.writeConfFiles(); err != nil {
		return retired, fmt.Errorf("写配置文件: %w", err)
	}
	return retired, a.pg.Start()
}

// ---- 通过 psql 操作数据库 -------------------------------------------------
//
// 直接用自带的 psql 而不是引入一个 Go 的 PostgreSQL 驱动：反正 psql 本来就在包里，
// 走 unix socket + trust 认证，管理壳完全不需要碰密码。

func (a *App) psql(ctx context.Context, db, sql string) (string, error) {
	cfg := a.config()
	cmd := a.pgCommand(ctx, "psql", a.PGData(),
		"--no-psqlrc", "--quiet", "--no-align", "--tuples-only",
		"--set=ON_ERROR_STOP=1",
		"--host="+a.sockDir, // 以 / 开头 → 走 unix socket，不做任何域名解析
		"--port="+strconv.Itoa(cfg.PGPort),
		"--username="+pgUser,
		"--dbname="+db,
		"--file=-", // SQL 走 stdin，不进命令行（可能含密码）
	)
	cmd.Stdin = strings.NewReader(sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("%s", msg)
	}
	return stdout.String(), nil
}

// ready 判断数据库是不是已经能接受连接了。
func (a *App) ready(ctx context.Context) bool {
	_, err := a.psql(ctx, "postgres", "SELECT 1;")
	return err == nil
}

// serverVersion 返回形如 "17.10" 的版本号。
func (a *App) serverVersion(ctx context.Context) string {
	out, err := a.psql(ctx, "postgres", "SHOW server_version;")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

type dbInfo struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Size  string `json:"size"`
	Enc   string `json:"encoding"`
}

func (a *App) listDatabases(ctx context.Context) ([]dbInfo, error) {
	const q = `SELECT d.datname, pg_catalog.pg_get_userbyid(d.datdba),
	                  pg_catalog.pg_size_pretty(pg_catalog.pg_database_size(d.datname)),
	                  pg_catalog.pg_encoding_to_char(d.encoding)
	           FROM pg_catalog.pg_database d
	           WHERE d.datallowconn AND NOT d.datistemplate
	           ORDER BY d.datname;`
	out, err := a.psql(ctx, "postgres", q)
	if err != nil {
		return nil, err
	}
	var list []dbInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 4 {
			continue
		}
		list = append(list, dbInfo{Name: f[0], Owner: f[1], Size: f[2], Enc: f[3]})
	}
	return list, nil
}

// quoteLiteral 把字符串安全地变成 SQL 字面量。
// standard_conforming_strings 默认是 on，所以反斜杠就是普通字符，只需要把单引号翻倍。
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// quoteIdent 把标识符安全地变成带双引号的形式。
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
