// MariaDB 的 UGOS Pro 原生应用管理壳。
//
// 为什么需要这么一层壳，而不是让 start_cmd 直接指向 mariadbd：
//
//  1. 绿联的 project.yaml 必须声明一个【HTTP】端口，平台靠它探活。
//     MariaDB 说的是 MySQL 协议，探不了活 —— 所以要有个 HTTP 服务顶在前面。
//  2. 数据目录首次要初始化（建系统表、设 root 密码）。上游的 mariadb-install-db
//     是个 shell 脚本，依赖 sh/sed/awk 一大串，而沙箱里没声明 EXEC 权限时
//     /bin 根本不存在、声明了也只有一份过滤过的白名单。这里改成直接调
//     `mariadbd --bootstrap` 把 SQL 喂进去，不依赖任何外部命令。
//  3. 数据库要优雅关闭（刷脏页），需要有人接管 SIGTERM 并等它退干净。
//
// 管理壳自己是纯标准库的 Go 静态二进制，零第三方依赖。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var logger = log.New(os.Stdout, "", log.LstdFlags)

func logf(format string, args ...any) {
	logger.Printf(format, args...)
}

// logWriter 把子进程的 stdout/stderr 逐行转进我们的日志。
type logWriter struct{ prefix string }

func (w logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			logf("[%s] %s", w.prefix, line)
		}
	}
	return len(p), nil
}

func main() {
	port := flag.Int("port", 23306, "管理页面的 HTTP 端口（须与 project.yaml 的 port 一致）")
	// 监听地址默认 0.0.0.0，安全性【不】靠它，靠的是 auth.go 里的两道闸
	// （请求源必须是本机 + 网关注入的身份必须是 admin）。
	//
	// 为什么不默认只绑 127.0.0.1：那样更简单也更强，但它成立的前提是
	// 【网关的 proxy_pass 走回环】—— 这一条没有真机验证过，赌输了应用直接打不开。
	// 源地址检查对"网关走回环"和"网关走本机 LAN IP"两种情况都成立。
	// 真机验证过网关确实走回环之后，把这里改成 127.0.0.1 会更好，两者不冲突。
	listen := flag.String("listen", "0.0.0.0",
		"管理页面的监听地址。收紧成 127.0.0.1 更好，但要先确认网关能连上，详见 auth.go")
	devNoAuth := flag.Bool("dev-no-auth", false,
		"仅供本地开发：跳过 UGOS 登录认证。绝不要在 NAS 上用")
	flag.Parse()

	if err := run(*listen, *port, *devNoAuth); err != nil {
		logf("::error:: %v", err)
		os.Exit(1)
	}
}

func run(listenAddr string, httpPort int, devNoAuth bool) error {
	paths, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := paths.Check(); err != nil {
		return err
	}
	logf("安装目录=%s 数据目录=%s 日志目录=%s", paths.Install, paths.Data, paths.Log)

	cfg, err := LoadConfig(paths.Data)
	if err != nil {
		return err
	}
	dataDir, err := cfg.ResolveDataDir(paths)
	if err != nil {
		return err
	}
	logf("数据库数据目录=%s", dataDir)

	srv := NewServer(paths, cfg, dataDir)
	if devNoAuth {
		logf("::warning:: 已用 --dev-no-auth 跳过 UGOS 登录认证。" +
			"这只该出现在本地开发环境 —— 任何能访问本端口的人都能看到数据库 root 密码。")
	}
	admin := NewAdmin(paths, cfg, srv, devNoAuth)

	mux := http.NewServeMux()
	admin.Register(mux)
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", listenAddr, httpPort),
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// 管理页面【先于】数据库起来。
	// 初始化或启动失败时，用户至少还能打开页面看到原因；
	// 直接退出的话，应用中心里只会显示一个"已停止"，没有任何线索。
	errCh := make(chan error, 1)
	go func() {
		logf("管理页面监听 %s:%d", listenAddr, httpPort)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	go startDatabase(cfg, srv, dataDir)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-errCh:
		srv.Close()
		return fmt.Errorf("管理页面服务异常退出: %w", err)
	case sig := <-sigCh:
		logf("收到信号 %v，开始关闭", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	// 关键：一定要等 mariadbd 真正退干净再返回，否则 InnoDB 可能来不及刷盘。
	srv.Close()
	logf("已关闭")
	return nil
}

// startDatabase 完成首次初始化并把数据库拉起来。放在后台跑，
// 因为初始化可能要几十秒，不能挡住管理页面上线。
func startDatabase(cfg *Config, srv *Server, dataDir string) {
	switch srv.InitState() {
	case InitComplete:
		// 正常路径，什么都不用做。

	case InitAccountsMissing:
		// 系统表在但 root 账号没建完 —— 0003 之前的版本装出来的库全是这个状态
		// （账号 SQL 被 bootstrap 的 skip-grant-tables 挡掉了，见 mariadb.go）。
		// 这里只补账号，不动任何已有数据，让存量机器升级后重启一次就自愈。
		pw := cfg.Snapshot().RootPassword
		if pw == "" {
			// 极端情况：配置丢了而数据还在。生成一个新的，用户到管理页看即可。
			p, err := GeneratePassword()
			if err != nil {
				logf("::error:: 生成 root 密码失败: %v", err)
				srv.setState(StateFailed, fmt.Sprintf("生成 root 密码失败: %v", err))
				return
			}
			if err := cfg.Update(func(d *ConfigData) error {
				d.RootPassword = p
				d.PasswordChangedByUser = false
				return nil
			}); err != nil {
				logf("::error:: 保存 root 密码失败: %v", err)
				srv.setState(StateFailed, fmt.Sprintf("保存 root 密码失败: %v", err))
				return
			}
			pw = p
		}
		if err := srv.RepairAccounts(pw); err != nil {
			logf("::error:: 补建 root 账号失败: %v", err)
			srv.setState(StateFailed, fmt.Sprintf("补建 root 账号失败: %v", err))
			return
		}

	case InitFresh:
		startFreshDatabase(cfg, srv)
		if srv.Status().State == StateFailed {
			return
		}
	}

	if err := srv.Start(); err != nil {
		logf("::error:: 启动失败: %v", err)
		return
	}
	// 小版本升级后补跑 mariadb-upgrade
	srv.MaybeUpgrade(cfg.Snapshot().RootPassword)
}

// startFreshDatabase 完成空目录的首次初始化。
// 失败时把状态置成 StateFailed，调用方据此决定不要继续启动。
func startFreshDatabase(cfg *Config, srv *Server) {
	// 首次启动：生成随机 root 密码。
	// 这里【不】依赖安装参数是否已经注入 —— 平台是先起服务、2~3 秒后才写
	// 参数和授权目录的，首启拿不到值是正常现象，ResolveDataDir 已经兜到
	// 应用数据目录，不会因此失败。
	var pw string
	err := cfg.Update(func(d *ConfigData) error {
		if d.RootPassword == "" {
			p, err := GeneratePassword()
			if err != nil {
				return err
			}
			d.RootPassword = p
			d.PasswordChangedByUser = false
		}
		pw = d.RootPassword
		return nil
	})
	if err != nil {
		logf("::error:: 准备 root 密码失败: %v", err)
		srv.setState(StateFailed, fmt.Sprintf("准备 root 密码失败: %v", err))
		return
	}
	if err := srv.Initialize(pw); err != nil {
		logf("::error:: 初始化失败: %v", err)
		srv.setState(StateFailed, fmt.Sprintf("数据目录初始化失败: %v", err))
		return
	}
}
