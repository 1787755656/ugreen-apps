package main

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (a *App) startHTTP(port int) *http.Server {
	mux := http.NewServeMux()

	// 探活用，【不能加鉴权】：系统探测是直接打端口的，不走网关，带不上用户头。
	mux.HandleFunc("/", a.handleRoot)
	// 鉴权链路自查，【故意不加鉴权】，见 ugauth.go 里 handleDiag 的说明。
	mux.HandleFunc("/api/diag", a.handleDiag)

	// 以下都要登录。
	// 【方法白名单不是洁癖】：会改状态或者很贵的接口一律只收 POST，
	// 详见 ugauth.go 里 postOnly 的注释。
	mux.HandleFunc("/api/me", a.auth(getOnly(a.handleMe)))
	mux.HandleFunc("/api/status", a.auth(getOnly(a.handleStatus)))
	mux.HandleFunc("/api/connection", a.auth(getOnly(a.handleConnection)))
	mux.HandleFunc("/api/logs", a.auth(getOnly(a.handleLogs)))
	mux.HandleFunc("/api/backups", a.auth(getOnly(a.handleBackups)))
	mux.HandleFunc("/api/backups/download", a.auth(getOnly(a.handleBackupDownload)))

	mux.HandleFunc("/api/superuser-password", a.auth(postOnly(a.handleSuperPassword)))
	mux.HandleFunc("/api/settings", a.auth(postOnly(a.handleSettings)))
	mux.HandleFunc("/api/server", a.auth(postOnly(a.handleServer)))
	mux.HandleFunc("/api/databases/drop", a.auth(postOnly(a.handleDropDatabase)))
	mux.HandleFunc("/api/backups/create", a.auth(postOnly(a.handleBackupCreate)))
	mux.HandleFunc("/api/backups/delete", a.auth(postOnly(a.handleBackupDelete)))
	mux.HandleFunc("/api/backups/restore", a.auth(postOnly(a.handleBackupRestore)))
	mux.HandleFunc("/api/backups/upload/init", a.auth(postOnly(a.handleUploadInit)))
	mux.HandleFunc("/api/backups/upload/chunk", a.auth(postOnly(a.handleUploadChunk)))
	mux.HandleFunc("/api/backups/upload/complete", a.auth(postOnly(a.handleUploadComplete)))
	mux.HandleFunc("/api/migrate", a.auth(postOnly(a.handleMigrate)))
	mux.HandleFunc("/api/reinit", a.auth(postOnly(a.handleReinit)))

	// 这两个 GET/POST 都收，各自在 handler 里分流。
	mux.HandleFunc("/api/databases", a.auth(a.handleDatabases))
	mux.HandleFunc("/api/custom-conf", a.auth(a.handleCustomConf))

	// 【只绑 127.0.0.1】—— 这一行是整套鉴权成立的前提，不是性能或习惯问题。
	// 鉴权完全依赖系统网关注入的 Ugreen-User-* 头，而 inner 应用的端口在局域网上
	// 是直接可达的（2026-08-06 真机实测），绑 0.0.0.0 就等于允许任何设备
	// 伪造那几个头、读到数据库密码。详见 ugauth.go 顶部。
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	go func() {
		log.Printf("管理页监听 %s（只绑回环，入口是系统网关）", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP 服务退出: %v", err)
		}
	}()
	return srv
}

func (a *App) shutdownHTTP(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// ---- 基础设施 -----------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	// 不认识的字段一律报错，不静默忽略 —— 否则前端字段名写错了会表现成
	// "设了没反应"，从外面完全看不出来。
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// handleRoot 只服务两个目的：给系统探活一个 200，以及在有人直连端口时说清楚。
//
// 前端页面【不由这里提供】：open_type: inner 的应用，页面是系统网关从
// rootfs_common/www/ 直接 serve 的，只有 /api 前缀的请求才会被反代到这里。
func (a *App) handleRoot(w http.ResponseWriter, r *http.Request) {
	// 【不是本机来的就什么都不说】。理论上绑了回环根本走不到这里，
	// 留着这一判断是为了让"信息不外泄"这件事不依赖于监听地址那一行代码。
	if !isSameHostRequest(r) {
		writeBlockedPage(w)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="zh-CN"><meta charset="utf-8">
<meta name="robots" content="noindex,nofollow">
<title>PostgreSQL</title><body style="font-family:sans-serif;padding:40px;line-height:1.7">
<h3>PostgreSQL 管理服务正在运行</h3>
<p>管理界面请从绿联云桌面里打开本应用，不要直接访问这个端口 ——
本服务只监听 127.0.0.1，鉴权依赖系统网关注入的登录信息。</p>
</body></html>`))
}

// handleMe 让页面知道当前是谁，顺便确认鉴权链路是通的。
func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := ugUserFrom(r)
	writeJSON(w, 200, map[string]any{
		"id": u.ID, "name": u.Name, "type": u.Type,
	})
}

// ---- 状态与设置 ---------------------------------------------------------

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	st := a.pg.Status()
	initSt, initWhy := classifyPGData(a.PGData())
	resp := map[string]any{
		"server":      st,
		"pg_port":     cfg.PGPort,
		"allow_lan":   cfg.AllowLAN,
		"timezone":    cfg.TimeZone,
		"app_dir":     a.dataDir,
		"data_dir":    a.PGData(),
		"initialized": initSt == InitReady,
		"data_broken": initSt == InitBroken,
		"data_state":  initWhy,
		"app_version": a.bundledVersion(),
		"migration":   a.mig.Snapshot(),
	}
	// "参数改了但数据还在老地方" —— 只有这时候才在页面上露出迁移卡片。
	// 平时不显示：迁移是个会停服几分钟的操作，不该长期摆在那里等人误点。
	if changed, target := a.pgBaseParamChanged(); changed {
		resp["migrate_to"] = target
	}
	if st.Running {
		resp["version"] = a.serverVersion(ctx)
	}
	writeJSON(w, 200, resp)
}

// handleMigrate 发起数据目录迁移。
//
// 【目标只能来自安装参数，不接受请求里传路径】。让前端传一个任意路径过来，
// 等于给了"把数据库搬到任何应用写得动的地方"这个能力；而参数那条路上平台已经
// 替我们把授权（BindPaths）办好了，传别的路径大概率也根本写不进去。
func (a *App) handleMigrate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClearTarget bool `json:"clear_target"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	changed, target := a.pgBaseParamChanged()
	if !changed {
		writeErr(w, 409, "安装参数里的数据目录和当前位置是一致的，不需要迁移。"+
			"要换位置请先在应用设置里改「数据保存目录」，改完重启一次应用。")
		return
	}
	if err := a.StartMigration(target, req.ClearTarget); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "to": target})
}

// handleReinit 清空并重新初始化一个状态异常的数据目录。旧目录只改名，不删除。
func (a *App) handleReinit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	// 要一个明确的确认字符串。这是整个应用里最接近"毁数据"的动作，
	// 不能只靠前端那个按钮拦着。
	if req.Confirm != "REINIT" {
		writeErr(w, 400, "缺少确认")
		return
	}
	retired, err := a.ReinitPGData()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "retired": retired})
}

// bundledVersion 读打包时写进去的 pgdg 版本号。
func (a *App) bundledVersion() string {
	buf, err := os.ReadFile(filepath.Join(a.pgRoot, "VERSION"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(buf))
}

func (a *App) handleConnection(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	// 主机地址【不由服务端给】：页面用 location.hostname 拼，那才是用户实际
	// 访问 NAS 用的地址。服务端这边既拿不准要给哪个网卡的 IP，也拿不准
	// 用户是走域名还是走 IP。
	writeJSON(w, 200, map[string]any{
		"user":     pgUser,
		"password": cfg.SuperPass,
		"port":     cfg.PGPort,
		"database": "postgres",
		"roles":    cfg.Roles,
	})
}

func (a *App) handleSuperPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"` // 空 = 随机生成一个
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	pw := req.Password
	if pw == "" {
		pw = randomPassword(20)
	} else if len(pw) < 8 {
		writeErr(w, 400, "数据库密码至少 8 位")
		return
	}
	if !a.pg.Status().Running {
		writeErr(w, 409, "数据库没在运行，先启动再改密码")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	sql := fmt.Sprintf("ALTER ROLE %s WITH PASSWORD %s;", quoteIdent(pgUser), quoteLiteral(pw))
	if _, err := a.psql(ctx, "postgres", sql); err != nil {
		writeErr(w, 500, "改密码失败："+err.Error())
		return
	}
	if err := a.updateConfig(func(c *Config) { c.SuperPass = pw }); err != nil {
		// 数据库那边已经改了，配置没存上 —— 必须让用户当场看到新密码，
		// 否则下次读配置拿到的是旧的，人就被锁在外面了。
		writeErr(w, 500, fmt.Sprintf(
			"数据库密码已改成 %s，但保存配置失败（%v）——请立刻记下这个密码", pw, err))
		return
	}
	writeJSON(w, 200, map[string]any{"password": pw})
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PGPort   int    `json:"pg_port"`
		AllowLAN bool   `json:"allow_lan"`
		TimeZone string `json:"timezone"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	// 【必须 >1024】：绿联原生应用跑在普通用户下，绑低位端口是
	// "bind: permission denied"，而且这个错要到重启之后才看得见。
	if req.PGPort < 1024 || req.PGPort > 65535 {
		writeErr(w, 400, "端口要在 1024~65535 之间（应用以普通用户运行，绑不了低位端口）")
		return
	}
	if req.TimeZone == "" {
		req.TimeZone = "Asia/Shanghai"
	}
	// 保存设置会顺带重启数据库，理由同 handleServer。
	if a.mig.Snapshot().Running {
		writeErr(w, 409, "正在迁移数据目录，请等它结束再改设置。")
		return
	}
	if _, err := os.Stat(filepath.Join(a.pgRoot, "zoneinfo", filepath.Clean(req.TimeZone))); err != nil {
		writeErr(w, 400, "时区 "+req.TimeZone+" 不在打包的时区数据里")
		return
	}
	if err := a.updateConfig(func(c *Config) {
		c.PGPort, c.AllowLAN, c.TimeZone = req.PGPort, req.AllowLAN, req.TimeZone
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	go func() {
		if err := a.pg.Restart(); err != nil {
			log.Printf("改设置后重启失败: %v", err)
		}
	}()
	writeJSON(w, 200, map[string]any{"ok": true, "restarting": true})
}

func (a *App) handleServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	// 迁移期间【绝不能让数据库起来】：它会在【旧】目录上启动并继续写，
	// 而我们正在把那个目录复制走 —— 复制出来的就是一份撕裂的数据。
	// 迁移结束时会自己把数据库拉起来，这里挡住就行。
	if a.mig.Snapshot().Running {
		writeErr(w, 409, "正在迁移数据目录，期间不能启停数据库。迁移完成后会自动启动。")
		return
	}
	switch req.Action {
	case "start":
		go func() {
			if err := a.bootPostgres(); err != nil {
				log.Printf("启动失败: %v", err)
			}
		}()
	case "stop":
		go a.pg.Stop()
	case "restart":
		go func() {
			if err := a.pg.Restart(); err != nil {
				log.Printf("重启失败: %v", err)
			}
		}()
	default:
		writeErr(w, 400, "action 只能是 start/stop/restart")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if n <= 0 || n > 2000 {
		n = 300
	}
	var lines []string
	if r.URL.Query().Get("src") == "app" {
		lines = a.rec.Lines()
	} else {
		lines = tailFile(a.logFile, n)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	writeJSON(w, 200, map[string]any{"lines": lines})
}

// tailFile 读文件末尾的若干行。只读最后 512KB —— 数据库日志可能很大，
// 全读进内存是不必要的风险。
func tailFile(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{"（还没有日志：" + err.Error() + "）"}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	const window = 512 << 10
	off := st.Size() - window
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	lines := splitLines(string(buf))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // 第一行多半是半截的，丢掉
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// ---- 数据库 -------------------------------------------------------------

// 数据库名/角色名只允许这个范围。PostgreSQL 本身允许的字符多得多，
// 但那些名字在连接串、备份文件名、各种客户端里都容易出岔子，
// 而 NAS 上建库的场景（给某个应用用）完全用不着奇怪的名字。
var nameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,62}$`)

func (a *App) handleDatabases(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if r.Method == http.MethodGet {
		list, err := a.listDatabases(ctx)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"items": list, "total": len(list)})
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 GET/POST")
		return
	}

	var req struct {
		Name       string `json:"name"`
		CreateRole bool   `json:"create_role"` // 顺手建一个同名的专用账号
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if !nameRe.MatchString(req.Name) {
		writeErr(w, 400, "名字只能用字母、数字和下划线，且以字母或下划线开头，最长 63 位")
		return
	}

	owner := pgUser
	rolePass := ""
	if req.CreateRole {
		rolePass = randomPassword(20)
		sql := fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s;",
			quoteIdent(req.Name), quoteLiteral(rolePass))
		if _, err := a.psql(ctx, "postgres", sql); err != nil {
			writeErr(w, 500, "建账号失败："+err.Error())
			return
		}
		owner = req.Name
	}
	// CREATE DATABASE 不能在事务块里跑，所以单独一条。
	sql := fmt.Sprintf("CREATE DATABASE %s OWNER %s;", quoteIdent(req.Name), quoteIdent(owner))
	if _, err := a.psql(ctx, "postgres", sql); err != nil {
		if req.CreateRole {
			// 库没建成就把刚建的账号收回去，别留半个残局。
			_, _ = a.psql(ctx, "postgres", "DROP ROLE IF EXISTS "+quoteIdent(req.Name)+";")
		}
		writeErr(w, 500, "建库失败："+err.Error())
		return
	}
	if req.CreateRole {
		_ = a.updateConfig(func(c *Config) { c.Roles[req.Name] = rolePass })
	}
	writeJSON(w, 200, map[string]any{"ok": true, "owner": owner, "password": rolePass})
}

func (a *App) handleDropDatabase(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		DropRole bool   `json:"drop_role"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if !nameRe.MatchString(req.Name) {
		writeErr(w, 400, "库名不合法")
		return
	}
	if req.Name == "postgres" {
		writeErr(w, 400, "postgres 是系统库，不能删")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	// WITH (FORCE) 会踢掉还连着的会话；没有它的话只要有个客户端连着就删不掉，
	// 而用户在页面上完全看不出是谁连着。
	sql := fmt.Sprintf("DROP DATABASE %s WITH (FORCE);", quoteIdent(req.Name))
	if _, err := a.psql(ctx, "postgres", sql); err != nil {
		writeErr(w, 500, "删库失败："+err.Error())
		return
	}
	if req.DropRole {
		if _, err := a.psql(ctx, "postgres", "DROP ROLE IF EXISTS "+quoteIdent(req.Name)+";"); err != nil {
			// 账号可能还拥有别的库里的对象，删不掉很正常，不该让整个操作失败。
			writeJSON(w, 200, map[string]any{
				"ok": true, "warning": "库已删除，但账号删不掉：" + err.Error()})
			return
		}
		_ = a.updateConfig(func(c *Config) { delete(c.Roles, req.Name) })
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- 备份 ---------------------------------------------------------------

type backupInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
}

// 备份文件名只允许我们自己生成的这种形状，用户传上来的也要过这一关。
// 这是防路径穿越的第一道闸（第二道是只用 filepath.Base 拼路径）。
var backupRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,120}\.sql\.gz$`)

func (a *App) backupPath(name string) (string, bool) {
	if !backupRe.MatchString(name) || strings.Contains(name, "..") {
		return "", false
	}
	return filepath.Join(a.backups, filepath.Base(name)), true
}

func (a *App) handleBackups(w http.ResponseWriter, r *http.Request) {
	ents, _ := os.ReadDir(a.backups)
	var list []backupInfo
	for _, e := range ents {
		if e.IsDir() || !backupRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, backupInfo{
			Name:  e.Name(),
			Size:  info.Size(),
			MTime: info.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name > list[j].Name })
	writeJSON(w, 200, map[string]any{"items": list, "total": len(list)})
}

func (a *App) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	if !a.pg.Status().Running {
		writeErr(w, 409, "数据库没在运行，没法备份")
		return
	}
	// 备份写在应用数据分区（NAS 系统盘）上。写满系统盘的后果远不止这个应用挂掉，
	// 所以宁可提前拒绝，也不要写到一半 ENOSPC 留下半个文件。
	//
	// 门槛取 512 MiB 而不是"数据库大小"：pg_dumpall 出来是 SQL 文本再 gzip，
	// 通常比数据文件小一个数量级，拿数据库体积当门槛会把大量正常备份挡掉。
	// 这里挡的是"盘已经快满了"，不是精确预测。
	const minFreeForBackup = 512 << 20
	if avail, err := freeSpace(a.backups); err == nil && avail < minFreeForBackup {
		writeErr(w, 507, fmt.Sprintf(
			"应用数据分区只剩 %s，低于 %s 就不开始备份了 —— 写满系统盘会影响整台 NAS。"+
				"请先删掉一些旧备份", humanBytes(int64(avail)), humanBytes(minFreeForBackup)))
		return
	}
	name := "dumpall-" + time.Now().Format("20060102-150405") + ".sql.gz"
	path := filepath.Join(a.backups, name)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	cfg := a.config()
	cmd := a.pgCommand(ctx, "pg_dumpall", a.PGData(),
		"--host="+a.sockDir,
		"--port="+strconv.Itoa(cfg.PGPort),
		"--username="+pgUser,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	// 先写 .part 再改名：失败时不留一个能被当成正常备份的半截文件。
	tmp := path + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	gz := gzip.NewWriter(f)

	if err := cmd.Start(); err != nil {
		gz.Close()
		f.Close()
		os.Remove(tmp)
		writeErr(w, 500, err.Error())
		return
	}
	_, copyErr := io.Copy(gz, stdout)
	waitErr := cmd.Wait()
	gzErr := gz.Close()
	closeErr := f.Close()

	if copyErr != nil || waitErr != nil || gzErr != nil || closeErr != nil {
		os.Remove(tmp)
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = fmt.Sprintf("copy=%v wait=%v gz=%v close=%v", copyErr, waitErr, gzErr, closeErr)
		}
		writeErr(w, 500, "备份失败："+msg)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	st, _ := os.Stat(path)
	var size int64
	if st != nil {
		size = st.Size()
	}
	writeJSON(w, 200, map[string]any{"name": name, "size": size})
}

func (a *App) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	path, ok := a.backupPath(r.URL.Query().Get("name"))
	if !ok {
		writeErr(w, 400, "备份文件名不合法")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, 404, "没有这个备份")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+filepath.Base(path)+`"`)
	if st != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}

func (a *App) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	path, ok := a.backupPath(req.Name)
	if !ok {
		writeErr(w, 400, "备份文件名不合法")
		return
	}
	if err := os.Remove(path); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- 上传备份（分片） ---------------------------------------------------
//
// 【必须分片】：inner 应用的请求要过 UGOS 网关，而网关全局 nginx.conf 里写死了
// client_max_body_size 20m，应用改不了它。一次性 POST 一个 100MB 的备份
// 必然 413。分片大小取 8MB，给请求头留足余量。
//
// 分片写成幂等的（同 index 重传就覆盖），于是天然支持断点续传。

const uploadChunkSize = 8 << 20

// maxUploadBytes 是单个上传备份的上限。
//
// 【必须有个上限】：没有它的话，一个能过鉴权的调用方可以一直 POST 分片，
// 把应用数据分区（NAS 的系统盘）撑爆 —— 系统盘满了不只是这个应用挂掉，
// 整台 NAS 都会出问题。128 GiB 远大于任何合理的 pg_dumpall.gz。
const maxUploadBytes int64 = 128 << 30

// uploadMaxChunks 由上限和分片大小推出来，别再单独写一个魔数。
const uploadMaxChunks = int(maxUploadBytes/uploadChunkSize) + 1

// uploadSessionTTL：超过这么久没完成的上传会话直接回收。
// 用户中途关掉页面是常事，不清理的话碎片会一直攒着。
const uploadSessionTTL = 24 * time.Hour

// uploadSessionID 只允许我们自己发的形状，防路径穿越。
var uploadSessRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (a *App) uploadDir(sessID string) (string, bool) {
	if !uploadSessRe.MatchString(sessID) {
		return "", false
	}
	return filepath.Join(a.uploads, sessID), true
}

// gcUploads 回收过期的上传会话。启动时跑一次，之后每小时一次。
func (a *App) gcUploads() {
	ents, err := os.ReadDir(a.uploads)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !uploadSessRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < uploadSessionTTL {
			continue
		}
		dir := filepath.Join(a.uploads, e.Name())
		if err := os.RemoveAll(dir); err == nil {
			log.Printf("回收了过期的上传会话 %s", e.Name())
		}
	}
}

func (a *App) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	// 目标路径在这一步就校验好，别等传完几百兆才发现文件名非法。
	if _, ok := a.backupPath(filepath.Base(req.Name)); !ok {
		writeErr(w, 400, "文件名必须形如 xxx.sql.gz")
		return
	}
	if req.Size <= 0 {
		writeErr(w, 400, "缺少文件大小")
		return
	}
	if req.Size > maxUploadBytes {
		writeErr(w, 400, fmt.Sprintf("文件太大了（%s），上限 %s",
			humanBytes(req.Size), humanBytes(maxUploadBytes)))
		return
	}
	// 分片会先落到应用数据目录、合并时再复制一份，所以峰值要 2 倍的空间。
	// 传到一半才 ENOSPC 的话用户白等，还留一地碎片。
	if avail, err := freeSpace(a.uploads); err == nil {
		if need := uint64(req.Size) * 2; avail < need {
			writeErr(w, 507, fmt.Sprintf(
				"空间不够：上传并合并需要约 %s，应用数据分区只剩 %s",
				humanBytes(int64(need)), humanBytes(int64(avail))))
			return
		}
	}
	a.gcUploads()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		writeErr(w, 500, "生成会话号失败")
		return
	}
	id := hex.EncodeToString(b)
	dir, _ := a.uploadDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "name"),
		[]byte(filepath.Base(req.Name)), 0o600); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"session": id, "chunk_size": uploadChunkSize})
}

func (a *App) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	dir, ok := a.uploadDir(r.URL.Query().Get("session"))
	if !ok {
		writeErr(w, 400, "会话号不合法")
		return
	}
	if _, err := os.Stat(dir); err != nil {
		writeErr(w, 404, "上传会话不存在或已过期")
		return
	}
	idx, err := strconv.Atoi(r.URL.Query().Get("index"))
	if err != nil || idx < 0 || idx >= uploadMaxChunks {
		writeErr(w, 400, "分片序号不合法")
		return
	}
	// 裸 body 流式落盘，不走 multipart —— 多一层封装只会让请求更大，
	// 服务端也不必把整片读进内存。
	part := filepath.Join(dir, fmt.Sprintf("%06d", idx))
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 多读一个字节：读得出来就说明这一片超了。
	// 【超了必须报错，不能默默截断】—— 截断出来的备份看着是成功的，
	// 直到有人拿它去还原才发现是坏的。
	n, copyErr := io.Copy(f, io.LimitReader(r.Body, uploadChunkSize+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(part)
		writeErr(w, 500, fmt.Sprintf("写分片失败：copy=%v close=%v", copyErr, closeErr))
		return
	}
	if n > uploadChunkSize {
		os.Remove(part)
		writeErr(w, 413, fmt.Sprintf("第 %d 片超过了 %s 的分片上限",
			idx, humanBytes(uploadChunkSize)))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "index": idx, "size": n})
}

func (a *App) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Session string `json:"session"`
		Chunks  int    `json:"chunks"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if req.Chunks <= 0 || req.Chunks > uploadMaxChunks {
		writeErr(w, 400, "分片数量不合法")
		return
	}
	dir, ok := a.uploadDir(req.Session)
	if !ok {
		writeErr(w, 400, "会话号不合法")
		return
	}
	nameBuf, err := os.ReadFile(filepath.Join(dir, "name"))
	if err != nil {
		writeErr(w, 404, "上传会话不存在或已过期")
		return
	}
	path, ok := a.backupPath(string(nameBuf))
	if !ok {
		writeErr(w, 400, "文件名不合法")
		return
	}

	// 【先确认每一片都在，再拼】。缺片直接失败、绝不产出半个文件 ——
	// 半个备份比明确失败糟糕得多：它看起来是个正常备份，直到有人拿它去还原。
	for i := 0; i < req.Chunks; i++ {
		if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%06d", i))); err != nil {
			writeErr(w, 400, fmt.Sprintf("缺少第 %d 片，请重新上传", i))
			return
		}
	}

	tmp := path + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var total int64
	for i := 0; i < req.Chunks; i++ {
		in, err := os.Open(filepath.Join(dir, fmt.Sprintf("%06d", i)))
		if err != nil {
			out.Close()
			os.Remove(tmp)
			writeErr(w, 500, err.Error())
			return
		}
		n, err := io.Copy(out, in)
		in.Close()
		if err != nil {
			out.Close()
			os.Remove(tmp)
			writeErr(w, 500, "合并失败："+err.Error())
			return
		}
		total += n
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	os.RemoveAll(dir)
	writeJSON(w, 200, map[string]any{"ok": true, "name": filepath.Base(path), "size": total})
}

func (a *App) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	path, ok := a.backupPath(req.Name)
	if !ok {
		writeErr(w, 400, "备份文件名不合法")
		return
	}
	if !a.pg.Status().Running {
		writeErr(w, 409, "数据库没在运行，没法还原")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, 404, "没有这个备份")
		return
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		writeErr(w, 400, "这个文件不是 gzip 格式："+err.Error())
		return
	}
	defer gz.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	cfg := a.config()
	// pg_dumpall 的产物是给 psql 吃的整脚本。这里【不加 ON_ERROR_STOP】：
	// 还原到一个已经有同名对象的库上时，"角色已存在"这类报错是预期内的，
	// 一遇错就停会让还原半途而废、留下更糟的中间状态。所以跑完再把
	// 全部输出交给用户自己判断。
	cmd := a.pgCommand(ctx, "psql", a.PGData(),
		"--no-psqlrc", "--quiet",
		"--host="+a.sockDir,
		"--port="+strconv.Itoa(cfg.PGPort),
		"--username="+pgUser,
		"--dbname=postgres",
		"--file=-",
	)
	cmd.Stdin = gz
	out, err := cmd.CombinedOutput()
	resp := map[string]any{"ok": err == nil, "output": tail(string(out), 20000)}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, 200, resp)
}

// ---- 自定义配置 ---------------------------------------------------------

func (a *App) handleCustomConf(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(a.PGData(), "postgresql.custom.conf")
	if r.Method == http.MethodGet {
		buf, err := os.ReadFile(path)
		if err != nil {
			buf = nil
		}
		writeJSON(w, 200, map[string]any{"content": string(buf)})
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if len(req.Content) > 256<<10 {
		writeErr(w, 400, "配置文件太大了")
		return
	}
	if err := os.WriteFile(path, []byte(req.Content), 0o600); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
