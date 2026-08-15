package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// Paths 是这个应用在沙箱里用到的全部路径。
//
// 安装目录（Root）在沙箱里是【只读】的，所以凡是要写的东西一律落在 Data / Cache 下。
// 上游默认把数据库放 /data、封面放 /covers、转码缓存放 /data/transcoded ——
// 那是 Docker 里的挂载点，绿联上都不存在，靠 config.toml 里的路径掰过来（见 config.go）。
type Paths struct {
	Root string // 安装目录，形如 /var/packages/<appid>
	Bin  string // <Root>/bin —— 上游二进制、ffmpeg/ffprobe/curl 和本管理壳都在这
	Lib  string // <Root>/lib —— libtag 等随包带的共享库
	App  string // <Root>/app —— 上游前端和 config.toml 模板

	Data  string // 可写。数据库、封面库、config.toml、pepper
	Cache string // 可写。转码缓存和 TMPDIR，掉了不心疼

	Server string // 上游二进制 zhiyin-music 的绝对路径
	Web    string // 前端构建产物目录（只读，给 config.toml 的 web.path 用）
}

// resolvePaths 定位自身安装目录并拼出其余路径。
//
// os.Executable() 在这里是可信的：unit 里 BindReadOnlyPaths=/proc/self 那个绑定
// 钉死在【主进程】的 pid 上，而管理壳自己就是 start_cmd 拉起的主进程，读到的正是自己。
// （会被这条坑到的是 fork 出去的子进程 —— 而我们走的是 exec，没有子进程。）
func resolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("定位自身可执行文件失败：%w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return Paths{}, fmt.Errorf("解析自身路径失败：%w", err)
	}
	root := filepath.Dir(filepath.Dir(exe)) // <root>/bin/zhiyin-launcher -> <root>

	p := Paths{
		Root: root,
		Bin:  filepath.Join(root, "bin"),
		Lib:  filepath.Join(root, "lib"),
		App:  filepath.Join(root, "app"),

		Data:  envOr("UGAPP_DATA_DIR", filepath.Join(root, "data")),
		Cache: envOr("UGAPP_CACHE_DIR", filepath.Join(root, "cache")),
	}
	p.Server = filepath.Join(p.Bin, "zhiyin-music")
	p.Web = filepath.Join(p.App, "web")
	return p, nil
}

// 数据目录下的几个落点。分 Data / Cache 的原则：删了要重新扫描全库的放 Data，
// 删了会自动重建的放 Cache。
func (p Paths) ConfigFile() string     { return filepath.Join(p.Data, "config.toml") }
func (p Paths) Database() string       { return filepath.Join(p.Data, "db.sqlite") }
func (p Paths) CoverStore() string     { return filepath.Join(p.Data, "covers") }
func (p Paths) PepperFile() string     { return filepath.Join(p.Data, ".zhiyin", "key_pepper") }
func (p Paths) RootsState() string     { return filepath.Join(p.Data, ".ugos-roots.json") }
func (p Paths) TranscodeCache() string { return filepath.Join(p.Cache, "transcoded") }

// TmpDir ——【沙箱里没有 /tmp】。上游是 Rust，落到 std::env::temp_dir() 的地方
// （封面写盘、刮削下载的临时文件）会拿到一个不存在的 /tmp。
func (p Paths) TmpDir() string { return filepath.Join(p.Cache, "tmp") }

// Home —— 沙箱里没有 /etc/passwd，靠 getpwuid 求 home 的代码会失败。
func (p Paths) Home() string { return filepath.Join(p.Data, "home") }

// ConfigTemplate 是随包带的上游 config.toml（就是镜像里的 /app/config.toml），
// 首次启动时按它生成用户的配置文件 —— 这样上游新增的配置项和中文注释都在。
func (p Paths) ConfigTemplate() string { return filepath.Join(p.App, "config.toml.default") }

// 上游的"更新日志"页面读的是【相对 cwd】的 releases/releases.json
// （镜像里是 /app/releases/releases.json，因为它的 cwd 就是 /app）。
// 我们的 cwd 是数据目录，所以要把随包那份放过去，否则日志里每次都是一句
// "读取 releases.json 失败"，界面上那一页空着。
func (p Paths) ReleasesSrc() string { return filepath.Join(p.App, "releases.json") }
func (p Paths) ReleasesDst() string { return filepath.Join(p.Data, "releases", "releases.json") }

// syncReleases 把随包的更新日志放到上游能读到的位置。每次启动都覆盖 ——
// 它是安装包自带的静态内容，应用升级后应该跟着更新。
func (p Paths) syncReleases() error {
	b, err := os.ReadFile(p.ReleasesSrc())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.ReleasesDst()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p.ReleasesDst(), b, 0o644)
}

func (p Paths) ensureDirs() error {
	for _, d := range []string{
		p.Data, p.Cache, p.CoverStore(), p.TranscodeCache(),
		p.TmpDir(), p.Home(), filepath.Dir(p.PepperFile()),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("建目录 %s 失败：%w", d, err)
		}
	}
	return nil
}

// checkPayload 在 exec 之前确认包内该有的东西都在。
//
// 缺任何一样，上游报出来的错都很难懂（动态库缺失是一句 ENOENT，
// 前端目录缺失则是打开页面 404），不如在这里当场说清楚缺的是哪个文件。
func (p Paths) checkPayload() error {
	for _, f := range []struct{ what, path string }{
		{"上游主程序 zhiyin-music", p.Server},
		{"TagLib 共享库 libtag.so.2", filepath.Join(p.Lib, "libtag.so.2")},
		{"TagLib 共享库 libtag_c.so.2", filepath.Join(p.Lib, "libtag_c.so.2")},
		{"前端页面 web/index.html", filepath.Join(p.Web, "index.html")},
		{"配置模板 config.toml.default", p.ConfigTemplate()},
	} {
		if _, err := os.Stat(f.path); err != nil {
			return fmt.Errorf("安装包不完整：找不到%s（%s）", f.what, f.path)
		}
	}
	return nil
}

// checkPortFree 在 exec 之前探一次端口。
//
// 用户很可能已经在同一台 NAS 上跑着 Docker 版的知音音乐或别的占了这个端口的东西。
// 上游撞上时只留一行 Rust 的 bind 报错就退出，应用中心那边只显示"未启动"，
// 用户完全不知道发生了什么 —— 所以这里要点名说是端口的问题。
func checkPortFree(port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("端口 %d 已经被别的程序占用了，本应用大概率起不来。"+
			"最常见的原因是这台 NAS 上还跑着 Docker 版的知音音乐，"+
			"把那个停掉、或者换一个端口再装（%v）", port, err)
	}
	return ln.Close()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
