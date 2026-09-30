package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// Admin 是管理页面的后端。
//
// 鉴权走 UGOS 的「登录认证」能力：网关校验登录态后注入 Ugreen-User-* 头。
// 两道闸（请求必须来自本机 + 必须带着网关注入的身份）都在 auth.go 里，
// 那边的注释解释了为什么【不能】拿 Ugreen-User-Type 判断管理员。
type Admin struct {
	paths     *Paths
	cfg       *Config
	srv       *Server
	devNoAuth bool // 只用于本地开发，绕过鉴权；启动时会打醒目警告

	mu     sync.Mutex
	lastOp Operation
}

// Operation 描述一次异步的耗时操作（启停、改密码）。
//
// 为什么要异步：停一个数据库最长要等 90 秒（InnoDB 刷脏页不能催），
// 而 UGOS 网关的反向代理读超时是 60 秒 —— 同步做的话请求会先被网关掐断，
// 用户看到的是"操作失败"，实际上后台正在正常执行。
type Operation struct {
	Kind    string `json:"kind"`
	Running bool   `json:"running"`
	// Phase 是操作内部当前走到哪一步（"正在复制…"之类）。
	// 迁移几个 G 的数据可能要好几分钟，只显示"运行中"的话用户无从判断
	// 是卡住了还是在正常干活 —— 而页面上还写着"别关页面"。
	Phase      string `json:"phase,omitempty"`
	Error      string `json:"error,omitempty"`
	Message    string `json:"message,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
}

func NewAdmin(p *Paths, cfg *Config, srv *Server, devNoAuth bool) *Admin {
	return &Admin{paths: p, cfg: cfg, srv: srv, devNoAuth: devNoAuth}
}

func (a *Admin) Register(mux *http.ServeMux) {
	// 网关【不会】剥掉 proxy_path 前缀，所以后端路由必须带 /api。
	// 凡是会吐敏感信息或改变状态的，一律套 requireAdmin。
	mux.HandleFunc("/api/status", a.requireAdmin(a.handleStatus))
	mux.HandleFunc("/api/password", a.requireAdmin(a.handlePassword))
	mux.HandleFunc("/api/network", a.requireAdmin(a.handleNetwork))
	mux.HandleFunc("/api/service", a.requireAdmin(a.handleService))
	mux.HandleFunc("/api/logs", a.requireAdmin(a.handleLogs))
	mux.HandleFunc("/api/migrate", a.requireAdmin(a.handleMigrate))

	// 自查接口不要求【网关认证】：鉴权链路不通时它就是唯一的排查手段，
	// 要求那一层等于把梯子架在门里面。它只回显调用方自己发来的头，不吐秘密。
	//
	// 但"必须来自本机"这道闸照样要过 —— 合法的排查请求本来就都从 NAS 上发起，
	// 加上它不损失任何排查能力，却能让局域网上的扫描者连"这里跑着个什么"都看不到。
	mux.HandleFunc("/api/diag", a.requireSameHost(a.handleDiag))

	// 探活接口【不】鉴权：平台靠它判断应用起没起来，而那个探测请求不经过网关，
	// 自然带不上 Ugreen-User-* 头。它只回一个 ok，不含任何信息。
	mux.HandleFunc("/api/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// 正常情况下页面由网关从 www/ 提供（open_type: inner 的静态资源走那条路）；
	// 这里自带一份主要是为了本地开发。
	//
	// ⚠ 这一份【必须设闸】。早先这里是裸的 FileServer，于是局域网上任何人
	// curl http://NAS:23306/ 都能拿到整份管理页 HTML —— 尽管 API 全都调不动，
	// 页面骨架本身已经把"这台 NAS 上跑着 MariaDB、能改哪些配置、连接串长什么样"
	// 全交代了。不是本机来的一律回一张什么都不说的 403 整页。
	if sub, err := fs.Sub(webFS, "web"); err == nil {
		mux.Handle("/", a.requirePageSameHost(http.FileServer(http.FS(sub))))
	}
}

// ---------- 异步操作 ----------

// begin 尝试开始一次异步操作。同一时刻只允许一个。
func (a *Admin) begin(kind string, fn func() (string, error)) error {
	a.mu.Lock()
	if a.lastOp.Running {
		running := a.lastOp.Kind
		a.mu.Unlock()
		return fmt.Errorf("正在执行「%s」，请等它完成", running)
	}
	a.lastOp = Operation{Kind: kind, Running: true}
	a.mu.Unlock()

	go func() {
		msg, err := fn()
		// ⚠ 结果一定要【也写进日志】。
		//
		// 早先它只存在内存里给管理页轮询，页面一刷新就没了 ——
		// 2026-08-09 排查一次失败的迁移时，日志里只能看到"开始迁移"和"数据库又起来了"，
		// 失败原因完全查不到，只能靠前后两次的文件数倒推。异步操作的失败尤其要留痕：
		// 用户看到的往往只是"点了没反应"或者"它自己又起来了"。
		if err != nil {
			logf("::error:: 「%s」失败：%v", kind, err)
		} else if msg != "" {
			logf("「%s」完成：%s", kind, msg)
		} else {
			logf("「%s」完成", kind)
		}
		a.mu.Lock()
		a.lastOp.Running = false
		a.lastOp.Message = msg
		a.lastOp.FinishedAt = time.Now().Unix()
		if err != nil {
			a.lastOp.Error = err.Error()
		} else {
			a.lastOp.Error = ""
		}
		a.mu.Unlock()
	}()
	return nil
}

// setPhase 更新当前操作的阶段说明。操作已经结束就不再改，
// 免得一条迟到的回调把最终结果覆盖掉。
func (a *Admin) setPhase(phase string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastOp.Running {
		a.lastOp.Phase = phase
	}
}

func (a *Admin) operation() Operation {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastOp
}

// ---------- 通用 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]any{"error": fmt.Sprintf(format, args...)})
}

// decodeBody 解析请求体。不认识的字段一律报错，不静默忽略 ——
// 静默忽略会让人以为设置生效了，从外面完全看不出来。
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求内容不合法: %w", err)
	}
	return nil
}

func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "该接口只接受 POST")
		return false
	}
	return true
}

// ---------- 状态 ----------

type statusResp struct {
	Server     Status    `json:"server"`
	Operation  Operation `json:"operation"`
	Port       int       `json:"port"`
	AllowLAN   bool      `json:"allow_lan"`
	BindAddr   string    `json:"bind_address"`
	DataDir    string    `json:"data_dir"`
	SocketPath string    `json:"socket_path"`
	ErrorLog   string    `json:"error_log"`
	Password   string    `json:"root_password"`
	PwChanged  bool      `json:"password_changed_by_user"`
	LANAddrs   []string  `json:"lan_addresses"`
	Warnings   []string  `json:"warnings"`
	// MigrateTo 非空表示"安装参数指向的位置和当前数据目录不一致"，
	// 前端据此显示「迁移数据目录」按钮。空字符串表示没什么可迁的。
	MigrateTo string `json:"migrate_to,omitempty"`
}

func (a *Admin) handleStatus(w http.ResponseWriter, r *http.Request) {
	d := a.cfg.Snapshot()
	resp := statusResp{
		Server:     a.srv.Status(),
		Operation:  a.operation(),
		Port:       d.Port,
		AllowLAN:   d.AllowLAN,
		BindAddr:   d.BindAddress(),
		DataDir:    a.srv.DataDir(),
		SocketPath: a.srv.socketPath(),
		ErrorLog:   a.srv.errorLogPath(),
		Password:   d.RootPassword,
		PwChanged:  d.PasswordChangedByUser,
		LANAddrs:   lanAddresses(),
		Warnings:   a.warnings(d),
	}
	if changed, param := a.cfg.DataDirParamChanged(); changed {
		resp.MigrateTo = TargetFromParam(param)
	}
	if resp.Server.Version == "" {
		resp.Server.Version = a.srv.Version()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Admin) warnings(d ConfigData) []string {
	out := []string{}
	if !d.PasswordChangedByUser {
		out = append(out, "root 密码还是首次启动自动生成的。它是随机的、强度足够，"+
			"但建议你改成自己记得住的；改过之后才能开启局域网访问。")
	}
	if changed, now := a.cfg.DataDirParamChanged(); changed {
		out = append(out, fmt.Sprintf(
			"安装参数里的数据目录已改成 %s，但数据库仍在 %s 运行。"+
				"点下面的「迁移数据目录」可以把数据搬过去（会短暂停库，"+
				"复制校验通过后才切换，旧数据会保留）。",
			TargetFromParam(now), a.srv.DataDir()))
	}
	if d.AllowLAN {
		out = append(out, fmt.Sprintf(
			"当前允许局域网访问：同网段的任何设备都可以连到 %d 端口。"+
				"请确认 root 密码足够强。", d.Port))
	}
	return out
}

// lanAddresses 列出本机的私网 IPv4，用来在页面上拼出可直接复制的连接串。
func lanAddresses() []string {
	out := []string{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() || !ip.IsPrivate() {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

// ---------- 改密码 ----------

type passwordReq struct {
	Password string `json:"password"`
	Generate bool   `json:"generate"`
}

// handleMigrate 把数据目录搬到安装参数当前指向的位置。
//
// 目标不接受调用方指定 —— 只认安装参数。理由：那个参数是用户在应用中心的
// 文件夹选择器里选的，选中的同时平台就把该目录授权进了沙箱（path_permissions）。
// 允许随便传路径的话，用户很容易填一个沙箱里根本写不进去的地方，
// 而失败要等到复制到一半才暴露。
func (a *Admin) handleMigrate(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var req struct{}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	param := paramDataDir()
	if param == "" {
		writeErr(w, http.StatusBadRequest,
			"安装参数里没有设置数据目录。请先到应用中心的「设置」里选一个共享文件夹，"+
				"然后把应用停止再启动一次（平台要重启后才会把新参数注入进来）。")
		return
	}
	target := TargetFromParam(param)
	// 先跑一遍校验，能当场拒绝的就别让用户等一次停库。
	if err := checkMigrationTarget(a.srv.DataDir(), target); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	err := a.begin("迁移数据目录", func() (string, error) {
		if err := a.srv.MigrateDataDir(target, a.setPhase); err != nil {
			return "", err
		}
		return fmt.Sprintf("数据已迁移到 %s，数据库已在新位置启动。"+
			"旧数据保留在原处（目录名加了 .migrated- 时间戳后缀），"+
			"确认一切正常后可以手动删除。", target), nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	// 202：操作已受理但还没做完，前端轮询 /api/status 里的 operation 看结果。
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

func (a *Admin) handlePassword(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var req passwordReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	newPw := req.Password
	byUser := true
	if req.Generate {
		if newPw != "" {
			writeErr(w, http.StatusBadRequest, "generate 和 password 只能给一个")
			return
		}
		var err error
		if newPw, err = GeneratePassword(); err != nil {
			writeErr(w, http.StatusInternalServerError, "%v", err)
			return
		}
		// 自动生成的不算"用户亲手改过"—— 局域网开关要的是用户确实知道这个密码。
		byUser = false
	}
	if err := ValidatePassword(newPw); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	err := a.begin("修改 root 密码", func() (string, error) {
		if err := a.srv.ChangeRootPassword(newPw); err != nil {
			return "", err
		}
		if err := a.cfg.Update(func(d *ConfigData) error {
			d.RootPassword = newPw
			d.PasswordChangedByUser = byUser
			return nil
		}); err != nil {
			// 数据库里已经改成功了，配置没存上 —— 必须把新密码带出来，
			// 否则用户既连不上（旧密码失效）、也看不到新密码。
			return "", fmt.Errorf("密码已在数据库里改好，但保存到配置文件失败：%v。"+
				"请立刻记下新密码：%s", err, newPw)
		}
		return "密码已修改，数据库已按新密码重启。记得同步更新用到它的应用。", nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	// 202：操作已受理但还没做完，前端轮询 /api/status 里的 operation 看结果。
	resp := map[string]any{"accepted": true}
	if req.Generate {
		resp["root_password"] = newPw
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// ---------- 网络设置 ----------

type networkReq struct {
	Port     *int  `json:"port"`
	AllowLAN *bool `json:"allow_lan"`
}

func (a *Admin) handleNetwork(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var req networkReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	if req.Port == nil && req.AllowLAN == nil {
		writeErr(w, http.StatusBadRequest, "至少要给 port 或 allow_lan 其中一个")
		return
	}

	err := a.cfg.Update(func(d *ConfigData) error {
		if req.Port != nil {
			p := *req.Port
			// <1024 在沙箱里绑不上（普通用户没有 CAP_NET_BIND_SERVICE），
			// 与其让它启动失败，不如在这里就说清楚。
			if p < 1024 || p > 65535 {
				return fmt.Errorf("端口必须在 1024–65535 之间：" +
					"应用以普通用户运行，绑不了 1024 以下的端口")
			}
			d.Port = p
		}
		if req.AllowLAN != nil {
			if *req.AllowLAN && !d.PasswordChangedByUser {
				return fmt.Errorf("请先把 root 密码改成你自己设定的，再开启局域网访问")
			}
			d.AllowLAN = *req.AllowLAN
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}

	// 端口和监听地址都写在 my.cnf 里，只有重启 mariadbd 才会生效。
	if !a.srv.Desired() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": false})
		return
	}
	if err := a.begin("按新的网络设置重启", func() (string, error) {
		if err := a.srv.Restart(); err != nil {
			return "", err
		}
		return "网络设置已生效。", nil
	}); err != nil {
		// 配置已经存了，只是这次没能顺带重启
		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted": true, "restarting": false,
			"note": fmt.Sprintf("设置已保存，但暂时无法重启（%v）。请稍后手动重启。", err),
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "restarting": true})
}

// ---------- 启停 ----------

type serviceReq struct {
	Action string `json:"action"`
}

func (a *Admin) handleService(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var req serviceReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	var kind string
	var fn func() error
	switch req.Action {
	case "start":
		kind, fn = "启动数据库", a.srv.Start
	case "stop":
		kind, fn = "停止数据库", a.srv.Stop
	case "restart":
		kind, fn = "重启数据库", a.srv.Restart
	default:
		writeErr(w, http.StatusBadRequest,
			"action 只能是 start / stop / restart，收到 %q", req.Action)
		return
	}

	if err := a.begin(kind, func() (string, error) {
		if err := fn(); err != nil {
			return "", err
		}
		return kind + "完成。", nil
	}); err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

// ---------- 日志 ----------

func (a *Admin) handleLogs(w http.ResponseWriter, r *http.Request) {
	const maxBytes = 256 * 1024
	path := a.srv.errorLogPath()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"content": "（错误日志还没有产生）"})
			return
		}
		writeErr(w, http.StatusInternalServerError, "读取日志失败：%v", err)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取日志失败：%v", err)
		return
	}
	// 只读文件尾部：错误日志跑久了可能很大，整份读进内存没有必要。
	offset := int64(0)
	if st.Size() > maxBytes {
		offset = st.Size() - maxBytes
	}
	if _, err := f.Seek(offset, 0); err != nil {
		writeErr(w, http.StatusInternalServerError, "读取日志失败：%v", err)
		return
	}
	buf := make([]byte, st.Size()-offset)
	n, _ := f.Read(buf)
	content := string(buf[:n])
	if offset > 0 {
		if i := strings.IndexByte(content, '\n'); i >= 0 {
			content = content[i+1:] // 丢掉被截断的半行
		}
		content = "…（已省略较早的日志）\n" + content
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": content})
}
