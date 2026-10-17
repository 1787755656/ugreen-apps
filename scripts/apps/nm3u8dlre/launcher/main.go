package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type server struct {
	paths *Paths
	store *taskStore
	upd   *updater
}

func main() {
	port := flag.Int("port", 28940, "HTTP 监听端口")
	flag.Parse()

	log.SetFlags(log.LstdFlags)

	// 自愈状态要在解析路径之前读（解析结果依赖"配额是否用尽"）。
	heal := loadHealState(filepath.Join(quickDataDir(), "selfheal.json"))

	p, err := resolvePaths(heal.exhausted())
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	s := &server{
		paths: p,
		store: newTaskStore(p),
		upd:   newUpdater(p.Data),
	}

	// 授权目录这次真的可用了 → 清掉自愈记录，下回从零开始
	if p.DownloadActive && (p.DownloadSource == "env" || p.DownloadSource == "unit") {
		heal.reset()
	}

	appendStartupRecord(p.Data, startupRecord{
		At:          nowISO(),
		EnvDir:      envDir("DOWNLOAD_DIR", ""),
		UnitPaths:   DeclaredDownloadDirs(p.Install),
		Download:    p.Download,
		Source:      p.DownloadSource,
		Active:      p.DownloadActive,
		Declared:    p.DeclaredPath,
		DeclaredErr: p.DeclaredErr,
		HealRecent:  heal.recent(),
		HealGaveUp:  heal.GaveUp,
		Uid:         os.Getuid(),
	}, 10)

	log.Printf("nm3u8dlre 启动: 安装=%s 数据=%s 下载=%s(%s,active=%v) 引擎=%s ffmpeg=%s",
		p.Install, p.Data, p.Download, p.DownloadSource, p.DownloadActive,
		p.Engine(), orNone(p.FFmpeg))
	if p.DeclaredPath != "" && !p.DownloadActive {
		log.Printf("[自愈] 平台已声明授权目录 %s，但沙箱里还用不了（%s）", p.DeclaredPath, p.DeclaredErr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api", s.handleAPI)
	mux.HandleFunc("/api/", s.handleAPI)
	mux.HandleFunc("/", s.handleStatic)

	srv := &http.Server{
		Addr:              net.JoinHostPort("0.0.0.0", strconv.Itoa(*port)),
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		log.Printf("收到停止信号，结束运行中的任务…")
		s.store.stopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	// 首启自愈：等 8 秒（平台写 unit 要 ~4 秒），授权目录仍不可用就主动退出一次，
	// 借 unit 的 Restart=on-failure + RestartSec=5s 重启，那时参数与挂载都已就位。
	go func() {
		time.Sleep(8 * time.Second)
		exit, reason := healDecision(p, heal)
		if !exit {
			return
		}
		log.Printf("[自愈] %s → 主动退出（%d 秒后由 systemd 拉起）", reason, 5)
		s.store.stopAll()
		os.Exit(healExitCode)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("监听失败: %v", err)
	}
	log.Printf("已退出")
}

func orNone(s string) string {
	if s == "" {
		return "(无)"
	}
	return s
}

type taskView struct {
	ID          string         `json:"id"`
	URL         string         `json:"url"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	Stage       string         `json:"stage"`
	Progress    float64        `json:"progress"`
	Speed       string         `json:"speed"`
	Segments    string         `json:"segments"`
	Downloaded  string         `json:"downloaded"`
	Total       string         `json:"total"`
	ETA         string         `json:"eta"`
	Stream      string         `json:"stream"`
	ExitCode    *int           `json:"exitCode"`
	CreatedAt   string         `json:"createdAt"`
	OutDir      string         `json:"outDir"`
	RealOutDir  string         `json:"realOutDir"`
	OutputFiles []string       `json:"outputFiles"`
	Options     map[string]any `json:"options"`
	RawParams   string         `json:"rawparams"`
	Last        string         `json:"last"`
}

func (s *server) view(t *Task) taskView {
	opts := map[string]any{
		"autoSelect":  t.Auto,
		"subOnly":     t.SubOnly,
		"live":        t.Live,
		"muxFormat":   t.Mux,
		"threadCount": nil,
	}
	if t.Threads > 0 {
		opts["threadCount"] = t.Threads
	}
	if t.OutDir != "" && t.OutDir != s.paths.Download {
		opts["saveDir"] = t.OutDir
	}

	v := taskView{
		ID: t.ID, URL: t.URL, Name: t.Name,
		Status: t.Status, ExitCode: t.ExitCode,
		CreatedAt: t.CreatedAt,
		OutDir:    t.OutDir,
		RealOutDir: realPath(t.OutDir),
		OutputFiles: t.OutputFiles,
		Options:    opts,
		RawParams:  t.RawParams,
		Stage:      "处理中",
	}
	if v.OutputFiles == nil {
		v.OutputFiles = []string{}
	}

	// 只有运行中/刚结束时才值得解析日志
	if t.Status == "running" || t.Status == "stopping" || t.Status == "success" || t.Status == "failed" || t.Status == "stopped" {
		pr := parseProgress(readLogTail(s.store.logPath(t.ID), 131072))
		v.Progress = pr.Percent
		v.Speed = pr.Speed
		v.Segments = pr.Segments
		v.Downloaded = pr.Downloaded
		v.Total = pr.Total
		v.ETA = pr.ETA
		v.Stream = pr.Stream
		v.Last = pr.Last
		v.Stage = pr.Stage
	}
	switch t.Status {
	case "success":
		v.Stage, v.Progress = "已完成", 100
	case "queued":
		v.Stage = "排队中"
	case "stopped":
		v.Stage = "已停止"
	case "failed":
		v.Stage = "失败"
	}
	return v
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, msg string) {
	writeJSON(w, 200, map[string]any{"ok": false, "error": msg})
}

func (s *server) handleAPI(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	action := r.FormValue("action")
	vals := map[string]string{}
	for k, v := range r.Form {
		if len(v) > 0 {
			vals[k] = v[len(v)-1]
		}
	}

	switch action {
	case "create":
		t, err := s.store.Create(vals, r.URL.RawQuery)
		if err != nil {
			errJSON(w, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "id": t.ID})

	case "status":
		tasks := s.store.list()
		views := make([]taskView, 0, len(tasks))
		for _, t := range tasks {
			views = append(views, s.view(t))
		}
		writeJSON(w, 200, map[string]any{"ok": true, "dir": s.paths.Download, "download": s.paths.downloadStatus(), "tasks": views})

	case "stop":
		if err := s.store.Stop(strings.TrimSpace(vals["id"])); err != nil {
			errJSON(w, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})

	case "remove":
		if err := s.store.Remove(strings.TrimSpace(vals["id"])); err != nil {
			errJSON(w, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})

	case "files":
		writeJSON(w, 200, map[string]any{"ok": true, "dir": s.paths.Download, "files": s.store.Files()})

	case "download":
		s.handleDownload(w, r)

	case "delete_file":
		s.handleDeleteFile(w, r)

	case "dirs":
		s.handleDirs(w, r)

	case "update_check":
		s.handleUpdateCheck(w, r)

	case "engine_update":
		s.handleEngineUpdate(w, r)

	case "app_download":
		errJSON(w, "应用本体请通过应用中心或应用商店更新")

	case "ping", "":
		if action == "" {
			s.handleStatic(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{
			"ok":         true,
			"app":        "N_m3u8DL-RE",
			"installDir": s.paths.Install,
			"dataDir":    s.paths.Data,
			"download":   s.paths.Download,
			"engine":     s.paths.Engine(),
			"engineOk":   fileExists(s.paths.Engine()),
			"ffmpeg":     s.paths.FFmpeg,
			"downloadInfo": s.paths.downloadStatus(),
			"wwwOk":      fileExists(filepath.Join(s.paths.WWW, "index.html")),
			"goArch":     runtime.GOARCH,
			"pid":        os.Getpid(),
			"uid":        os.Getuid(),
		})

	default:
		errJSON(w, "未知操作: "+action)
	}
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	// 允许把 action 直接挂在根路径上（tab 模式或本地直连调试）
	if r.URL.Query().Get("action") != "" {
		s.handleAPI(w, r)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if rel == "" {
		rel = "index.html"
	}
	if strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.paths.WWW, rel)
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(rel, ".html"):
		w.Header().Set("Cache-Control", "no-store")
	case strings.HasSuffix(rel, ".js"), strings.HasSuffix(rel, ".css"):
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFile(w, r, full)
}
