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

	mux.HandleFunc("/api/password", a.auth(postOnly(a.handlePassword)))
	mux.HandleFunc("/api/settings", a.auth(postOnly(a.handleSettings)))
	mux.HandleFunc("/api/server", a.auth(postOnly(a.handleServer)))
	mux.HandleFunc("/api/command", a.auth(postOnly(a.handleCommand)))
	mux.HandleFunc("/api/backups/create", a.auth(postOnly(a.handleBackupCreate)))
	mux.HandleFunc("/api/backups/delete", a.auth(postOnly(a.handleBackupDelete)))
	mux.HandleFunc("/api/backups/restore", a.auth(postOnly(a.handleBackupRestore)))
	mux.HandleFunc("/api/backups/upload/init", a.auth(postOnly(a.handleUploadInit)))
	mux.HandleFunc("/api/backups/upload/chunk", a.auth(postOnly(a.handleUploadChunk)))
	mux.HandleFunc("/api/backups/upload/complete", a.auth(postOnly(a.handleUploadComplete)))
	mux.HandleFunc("/api/migrate", a.auth(postOnly(a.handleMigrate)))
	mux.HandleFunc("/api/migrate/dismiss", a.auth(postOnly(a.handleMigrateDismiss)))

	mux.HandleFunc("/api/custom-conf", a.auth(a.handleCustomConf))

	// 【只绑 127.0.0.1】—— 这一行是整套鉴权成立的前提，不是性能或习惯问题。
	// 鉴权完全依赖系统网关注入的 Ugreen-User-* 头，而 inner 应用的端口在局域网上
	// 是直接可达的（2026-08-06 真机实测），绑 0.0.0.0 就等于允许任何设备
	// 伪造那几个头、读到 Redis 密码。详见 ugauth.go 顶部。
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
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
<title>Redis</title><body style="font-family:sans-serif;padding:40px;line-height:1.7">
<h3>Redis 管理服务正在运行</h3>
<p>管理界面请从绿联云桌面里打开本应用，不要直接访问这个端口 ——
本服务只监听 127.0.0.1，鉴权依赖系统网关注入的登录信息。</p>
</body></html>`))
}

// handleMe 让页面知道当前是谁，顺便确认鉴权链路是通的。
func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := ugUserFrom(r)
	writeJSON(w, 200, map[string]any{"id": u.ID, "name": u.Name, "type": u.Type})
}

// ---- 状态 ---------------------------------------------------------------

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	st := a.rd.Status()
	resp := map[string]any{
		"server":            st,
		"port":              cfg.Port,
		"allow_lan":         cfg.AllowLAN,
		"appendonly":        cfg.AppendOnly,
		"maxmemory_mb":      cfg.MaxMemoryMB,
		"maxmemory_policy":  cfg.MaxMemPolicy,
		"policies":          maxmemoryPolicies,
		"databases":         cfg.Databases,
		"load_modules":      cfg.LoadModules,
		"modules_available": a.availableModules(),
		"modules_fallback":  cfg.ModulesDisabledByFallback,
		"cow_fallback":      cfg.IgnoreARM64COWBug,
		"data_dir":          a.DataBase(),
		"app_dir":           a.dataDir,
		"total_mem_mb":      totalMemoryMB(),
		"app_version":       a.bundledVersion(),
		"migration":         a.mig.Snapshot(),
	}
	// "参数改了但数据还在老地方" —— 只有这时候才在页面上露出迁移卡片。
	if changed, target := a.dataBaseParamChanged(); changed {
		resp["migrate_to"] = target
	}

	if st.Running {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if info, err := a.info(ctx, ""); err == nil {
			resp["version"] = info["redis_version"]
			resp["stats"] = summarize(info)
			resp["keyspace"] = ParseKeyspace(info)
		} else {
			// 进程活着但连不上也是一种状态，要说出来，不能显示成"一切正常"
			resp["info_error"] = err.Error()
		}
	}
	writeJSON(w, 200, resp)
}

// summarize 从 INFO 里挑出管理页真正要显示的那些。
//
// 挑而不是全给：INFO 有两百多项，全丢给前端只会让页面变成一堆噪音，
// 而且每 5 秒传一次也不必要。
func summarize(info map[string]string) map[string]string {
	hits, _ := strconv.ParseFloat(info["keyspace_hits"], 64)
	misses, _ := strconv.ParseFloat(info["keyspace_misses"], 64)
	hitRate := "—"
	if hits+misses > 0 {
		hitRate = fmt.Sprintf("%.1f%%", hits/(hits+misses)*100)
	}
	out := map[string]string{
		"used_memory":       humanBytesStr(info["used_memory"]),
		"used_memory_peak":  humanBytesStr(info["used_memory_peak"]),
		"used_memory_rss":   humanBytesStr(info["used_memory_rss"]),
		"maxmemory":         humanBytesStr(info["maxmemory"]),
		"connected_clients": info["connected_clients"],
		"uptime_days":       info["uptime_in_days"],
		"hit_rate":          hitRate,
		"total_commands":    info["total_commands_processed"],
		"aof_enabled":       info["aof_enabled"],
		"rdb_last_status":   info["rdb_last_bgsave_status"],
		"rdb_changes":       info["rdb_changes_since_last_save"],
		"evicted_keys":      info["evicted_keys"],
		"expired_keys":      info["expired_keys"],
		"blocked_clients":   info["blocked_clients"],
		"role":              info["role"],
	}
	if ts, err := strconv.ParseInt(info["rdb_last_save_time"], 10, 64); err == nil && ts > 0 {
		out["rdb_last_save"] = time.Unix(ts, 0).Format("2006-01-02 15:04:05")
	}
	return out
}

// bundledVersion 读打包时写进去的上游版本号。
func (a *App) bundledVersion() string {
	buf, err := os.ReadFile(filepath.Join(a.redisDir, "VERSION"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(buf))
}

func (a *App) handleConnection(w http.ResponseWriter, r *http.Request) {
	cfg := a.config()
	// 主机地址【不由服务端给】：页面用 location.hostname 拼，那才是用户实际
	// 访问 NAS 用的地址。服务端这边既拿不准要给哪个网卡的 IP，
	// 也拿不准用户是走域名还是走 IP。
	writeJSON(w, 200, map[string]any{
		"password":  cfg.Password,
		"port":      cfg.Port,
		"allow_lan": cfg.AllowLAN,
	})
}

func (a *App) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"` // 空 = 随机生成一个
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	pw := req.Password
	if pw == "" {
		pw = randomPassword(24)
	} else if len(pw) < 8 {
		writeErr(w, 400, "密码至少 8 位")
		return
	}
	// Redis 的配置文件里密码是带引号的字符串，换行会把配置写坏。
	if strings.ContainsAny(pw, "\r\n") {
		writeErr(w, 400, "密码里不能有换行")
		return
	}
	if a.mig.Snapshot().Running {
		writeErr(w, 409, "正在迁移数据目录，请等它结束再改密码")
		return
	}

	// 顺序很重要：先落盘、再让运行中的实例生效、最后换管理壳自己的凭据。
	//
	// 反过来（先 CONFIG SET）的话，配置落盘失败时实际密码已经变了、
	// 而 config.json 里还是旧的 —— 用户就被锁在外面了，页面上显示的密码是错的。
	if err := a.updateConfig(func(c *Config) { c.Password = pw }); err != nil {
		writeErr(w, 500, "保存密码失败："+err.Error())
		return
	}
	if err := a.writeConf(); err != nil {
		writeErr(w, 500, "写配置文件失败："+err.Error())
		return
	}
	if a.rd.Status().Running {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		// CONFIG SET requirepass 立刻生效，不用重启 —— 重启会断开所有客户端，
		// 对一个被别的应用当缓存用的 Redis 来说代价太大。
		if _, err := a.rds.Do(ctx, "CONFIG", "SET", "requirepass", pw); err != nil {
			writeErr(w, 500, fmt.Sprintf(
				"新密码已经存进配置（重启后生效），但让运行中的实例立刻生效失败：%v", err))
			return
		}
	}
	// 管理壳自己的连接凭据最后换。换早了上面那条 CONFIG SET 就发不出去。
	a.rds.SetAuth(pw)
	writeJSON(w, 200, map[string]any{"password": pw})
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port         int    `json:"port"`
		AllowLAN     bool   `json:"allow_lan"`
		AppendOnly   bool   `json:"appendonly"`
		MaxMemoryMB  int    `json:"maxmemory_mb"`
		MaxMemPolicy string `json:"maxmemory_policy"`
		LoadModules  bool   `json:"load_modules"`
		Databases    int    `json:"databases"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	// 【必须 >1024】：绿联原生应用跑在普通用户下，绑低位端口是
	// "bind: permission denied"，而且这个错要到重启之后才看得见。
	if req.Port < 1024 || req.Port > 65535 {
		writeErr(w, 400, "端口要在 1024~65535 之间（应用以普通用户运行，绑不了低位端口）")
		return
	}
	if !validPolicy(req.MaxMemPolicy) {
		writeErr(w, 400, "淘汰策略只能是："+strings.Join(maxmemoryPolicies, " / "))
		return
	}
	if req.MaxMemoryMB < 0 {
		writeErr(w, 400, "内存上限不能是负数")
		return
	}
	if req.Databases < 1 || req.Databases > 1024 {
		writeErr(w, 400, "数据库数量要在 1~1024 之间")
		return
	}
	if a.mig.Snapshot().Running {
		writeErr(w, 409, "正在迁移数据目录，请等它结束再改设置")
		return
	}

	if err := a.updateConfig(func(c *Config) {
		c.Port, c.AllowLAN = req.Port, req.AllowLAN
		c.AppendOnly = req.AppendOnly
		c.MaxMemoryMB, c.MaxMemPolicy = req.MaxMemoryMB, req.MaxMemPolicy
		c.Databases = req.Databases
		if req.LoadModules != c.LoadModules {
			// 用户手动改过之后，"曾经因为加载失败被自动关掉"这件事就不再需要提示了
			c.ModulesDisabledByFallback = false
		}
		c.LoadModules = req.LoadModules
	}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	go func() {
		if err := a.rd.Restart(); err != nil {
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
	// 迁移期间【绝不能让 Redis 起来】：它会在【旧】目录上启动并继续写，
	// 而我们正在把那个目录复制走 —— 复制出来的就是一份撕裂的数据。
	if a.mig.Snapshot().Running {
		writeErr(w, 409, "正在迁移数据目录，期间不能启停 Redis。迁移完成后会自动启动。")
		return
	}
	switch req.Action {
	case "start":
		go func() {
			if err := a.bootRedis(); err != nil {
				log.Printf("启动失败: %v", err)
			}
		}()
	case "stop":
		go a.rd.Stop()
	case "restart":
		go func() {
			if err := a.rd.Restart(); err != nil {
				log.Printf("重启失败: %v", err)
			}
		}()
	default:
		writeErr(w, 400, "action 只能是 start/stop/restart")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleCommand 是管理页上的「运行命令」。
//
// 有它的理由很实在：Redis 没有 phpMyAdmin 那样的通用客户端，而 NAS 用户
// 手上没有 SSH、也装不了 redis-cli。没有这个框，用户连"看看某个 key 在不在"
// 都做不到，这个应用就只是个开关。
//
// 挡的东西见 redis.go 里 blockedCommands 的说明 —— 挡的不是"危险"，
// 是"会让管理壳和实际状态分家"或者"会把连接挂死"的那些。
func (a *App) handleCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Line string `json:"line"`
		DB   int    `json:"db"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if len(req.Line) > 64<<10 {
		writeErr(w, 400, "命令太长了")
		return
	}
	args, err := SplitArgs(req.Line)
	if err != nil {
		writeErr(w, 400, "命令解析失败："+err.Error())
		return
	}
	if len(args) == 0 {
		writeErr(w, 400, "空命令")
		return
	}
	if err := checkCommand(args); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !a.rd.Status().Running {
		writeErr(w, 409, "Redis 没在运行")
		return
	}
	cfg := a.config()
	if req.DB < 0 || req.DB >= cfg.Databases {
		writeErr(w, 400, fmt.Sprintf("库号要在 0~%d 之间", cfg.Databases-1))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// 每次都显式 SELECT：管理壳只有一条连接，上一条命令可能把它留在别的库上。
	if _, err := a.rds.Do(ctx, "SELECT", strconv.Itoa(req.DB)); err != nil {
		writeErr(w, 500, "切换数据库失败："+err.Error())
		return
	}
	v, err := a.rds.Do(ctx, args...)
	if err != nil {
		// 服务端明确回的错误是【正常结果】，不是 HTTP 层面的失败 ——
		// 用户敲错命令时应该在结果框里看到 Redis 的原话，而不是一个红色的报错条。
		if isRedisError(err) {
			writeJSON(w, 200, map[string]any{"output": "(error) " + err.Error(), "failed": true})
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"output": v.String()})
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

// tailFile 读文件末尾的若干行。只读最后 512KB —— 日志可能很大，
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

// ---- 备份 ---------------------------------------------------------------

type backupInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
}

// 备份文件名只允许我们自己生成的这种形状，用户传上来的也要过这一关。
// 这是防路径穿越的第一道闸（第二道是只用 filepath.Base 拼路径）。
var backupRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,120}\.rdb\.gz$`)

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
	if !a.rd.Status().Running {
		writeErr(w, 409, "Redis 没在运行，没法备份")
		return
	}
	// 备份写在应用数据分区（NAS 系统盘）上。写满系统盘的后果远不止这个应用挂掉。
	const minFree = 512 << 20
	if avail, err := freeSpace(a.backups); err == nil && avail < minFree {
		writeErr(w, 507, fmt.Sprintf(
			"应用数据分区只剩 %s，低于 %s 就不开始备份了 —— 写满系统盘会影响整台 NAS。"+
				"请先删掉一些旧备份", humanBytes(int64(avail)), humanBytes(minFree)))
		return
	}

	// 【必须先 BGSAVE 并等它完成】：直接复制 dump.rdb 拿到的是上一次存盘的
	// 老数据，甚至是正在被写的半截文件 —— 而它看起来是个正常备份，
	// 直到有人拿它去还原。bgsave() 会等 rdb_bgsave_in_progress 落下来。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := a.bgsave(ctx); err != nil {
		writeErr(w, 500, "存盘失败："+err.Error())
		return
	}

	name := "dump-" + time.Now().Format("20060102-150405") + ".rdb.gz"
	path := filepath.Join(a.backups, name)
	tmp := path + ".part"

	src, err := os.Open(a.rdbPath())
	if err != nil {
		writeErr(w, 500, "读不到 dump.rdb（"+err.Error()+"）——"+
			"如果这是一个刚建好、还没写过任何数据的 Redis，先写一个 key 再备份")
		return
	}
	defer src.Close()

	// 先写 .part 再改名：失败时不留一个能被当成正常备份的半截文件。
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	gz := gzip.NewWriter(out)
	_, copyErr := io.Copy(gz, src)
	gzErr := gz.Close()
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || gzErr != nil || syncErr != nil || closeErr != nil {
		os.Remove(tmp)
		writeErr(w, 500, fmt.Sprintf("备份失败：copy=%v gz=%v sync=%v close=%v",
			copyErr, gzErr, syncErr, closeErr))
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
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
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

// handleBackupRestore 从备份还原。
//
// 【AOF 是这里最大的坑】：appendonly yes 的时候 Redis 启动【只看 AOF，不看
// dump.rdb】。所以"把 dump.rdb 换掉再启动"这个天真的做法什么都不会发生 ——
// 页面显示还原成功，数据却一点没变，而且没有任何报错。
// 正确的做法是把 appendonlydir 一起挪开：AOF 清单不存在时 Redis 会
// 从 RDB 载入，然后照着载入的数据重建一份新的 AOF。
//
// 挪开的东西【一律改名保留】，不删除 —— 还原本身就是"我搞砸了想回退"的操作，
// 在这条路径上再毁一次数据是不可接受的。
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
	if a.mig.Snapshot().Running {
		writeErr(w, 409, "正在迁移数据目录，请等它结束")
		return
	}
	if _, err := os.Stat(path); err != nil {
		writeErr(w, 404, "没有这个备份")
		return
	}

	// 1) 先在临时文件里解开并【用 redis-check-rdb 验一遍】，再动现有数据。
	//    验不过的备份根本不该走到"停服"这一步。
	stage := filepath.Join(a.dataDir, "tmp", "restore.rdb")
	if err := gunzipTo(path, stage); err != nil {
		writeErr(w, 400, "解压备份失败："+err.Error())
		return
	}
	defer os.Remove(stage)
	if out, err := a.checkRDB(stage); err != nil {
		writeErr(w, 400, "这个备份没通过 redis-check-rdb 校验，已放弃还原（现有数据没有被动过）：\n"+out)
		return
	}

	// 2) 停服。
	wasRunning := a.rd.Status().Running
	a.rd.Stop()

	stamp := time.Now().Format("20060102-150405")
	base := a.DataBase()
	var moved []string
	restore := func() { // 出错时把挪开的东西放回去
		for _, p := range moved {
			orig := strings.TrimSuffix(p, ".before-restore-"+stamp)
			_ = os.Rename(p, orig)
		}
	}
	for _, name := range []string{"dump.rdb", "appendonlydir"} {
		src := filepath.Join(base, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := src + ".before-restore-" + stamp
		if err := os.Rename(src, dst); err != nil {
			restore()
			if wasRunning {
				_ = a.bootRedis()
			}
			writeErr(w, 500, fmt.Sprintf("把现有的 %s 挪开时失败：%v（数据没有被动过）", name, err))
			return
		}
		moved = append(moved, dst)
	}

	// 3) 放进新的 dump.rdb。
	if err := copyFile(stage, a.rdbPath(), 0o600); err != nil {
		_ = os.Remove(a.rdbPath())
		restore()
		if wasRunning {
			_ = a.bootRedis()
		}
		writeErr(w, 500, "写入还原数据失败："+err.Error()+"（原数据已放回）")
		return
	}

	// 4) 起服。
	if err := a.bootRedis(); err != nil {
		writeErr(w, 500, fmt.Sprintf(
			"数据已换成备份内容，但 Redis 启动失败：%v。原来的数据还在 %s 里（后缀 .before-restore-%s），"+
				"可以手动改回去", err, base, stamp))
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"note": fmt.Sprintf("还原完成。原来的数据没有删除，改名保留在 %s 里（后缀 .before-restore-%s），"+
			"确认无误后可以自行清理", base, stamp),
	})
}

func gunzipTo(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("不是 gzip 格式：%w", err)
	}
	defer gz.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// 解压有上限：gzip 炸弹能把应用数据分区填满
	if _, err := io.Copy(out, io.LimitReader(gz, maxUploadBytes)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// checkRDB 用自带的 redis-check-rdb 校验一个 RDB 文件。
// 它其实就是 redis-server 本身，靠 argv[0] 分流（见 scripts/fetch-runtime.py）。
func (a *App) checkRDB(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := a.redisCommand(ctx, "redis-check-rdb", path)
	out, err := cmd.CombinedOutput()
	return tail(string(out), 4000), err
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ---- 上传备份（分片） ---------------------------------------------------
//
// 【必须分片】：inner 应用的请求要过 UGOS 网关，而网关全局 nginx.conf 里写死了
// client_max_body_size 20m，应用改不了它。一次性 POST 一个 100MB 的备份必然 413。
// 分片写成幂等的（同 index 重传就覆盖），于是天然支持断点续传。

const uploadChunkSize = 8 << 20

// maxUploadBytes 是单个上传备份的上限。没有它的话，一个能过鉴权的调用方
// 可以一直 POST 分片，把 NAS 的系统盘撑爆 —— 那不只是这个应用挂掉。
const maxUploadBytes int64 = 64 << 30

const uploadMaxChunks = int(maxUploadBytes/uploadChunkSize) + 1

// uploadSessionTTL：超过这么久没完成的上传会话直接回收。
// 用户中途关掉页面是常事，不清理的话碎片会一直攒着。
const uploadSessionTTL = 24 * time.Hour

var uploadSessRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (a *App) uploadDir(sessID string) (string, bool) {
	if !uploadSessRe.MatchString(sessID) {
		return "", false
	}
	return filepath.Join(a.uploads, sessID), true
}

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
		if err := os.RemoveAll(filepath.Join(a.uploads, e.Name())); err == nil {
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
		writeErr(w, 400, "文件名必须形如 xxx.rdb.gz")
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
	// 分片会先落盘、合并时再复制一份，所以峰值要 2 倍的空间。
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
	// 【超了必须报错，不能默默截断】—— 截断出来的备份看着是成功的。
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

// ---- 迁移 ---------------------------------------------------------------

// handleMigrate 发起数据目录迁移。
//
// 【目标只能来自安装参数，不接受请求里传路径】。让前端传一个任意路径过来，
// 等于给了"把数据搬到任何应用写得动的地方"这个能力；而参数那条路上平台已经
// 替我们把授权（BindPaths）办好了，传别的路径大概率也根本写不进去。
func (a *App) handleMigrate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClearTarget bool `json:"clear_target"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	changed, target := a.dataBaseParamChanged()
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

// handleMigrateDismiss 把上一次迁移的结果收起来。
//
// 有它的原因很具体：迁移成功之后"参数和实际位置不一致"这个条件就不成立了，
// 但结果还得让用户看见，所以卡片在 finished 时也显示 —— 少了这个入口
// （和 migrateResultTTL 那个兜底），那张卡片就永远挂在页面上。真机上就是这样。
func (a *App) handleMigrateDismiss(w http.ResponseWriter, r *http.Request) {
	a.mig.Dismiss()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- 自定义配置 ---------------------------------------------------------

func (a *App) handleCustomConf(w http.ResponseWriter, r *http.Request) {
	path := a.customConfPath()
	if r.Method == http.MethodGet {
		buf, err := os.ReadFile(path)
		if err != nil {
			buf = nil
		}
		writeJSON(w, 200, map[string]any{"content": string(buf)})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeErr(w, 405, "只支持 GET/POST")
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
