package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	engineRepo = "nilaoda/N_m3u8DL-RE"
	ghTimeout  = 20 * time.Second
)

type ghAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
	ID   int64  `json:"id"`
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	PublishedAt string    `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []ghAsset `json:"assets"`
}

// updateJob 是引擎/应用后台更新的进度记录（落盘，重启后仍可读）。
type updateJob struct {
	Mode          string `json:"mode"`
	Status        string `json:"status"` // running ok failed
	TargetVersion string `json:"targetVersion,omitempty"`
	StartedAt     string `json:"startedAt,omitempty"`
	FinishedAt    string `json:"finishedAt,omitempty"`
	Message       string `json:"message,omitempty"`
	Name          string `json:"name,omitempty"`
	Path          string `json:"path,omitempty"`
	Size          int64  `json:"size,omitempty"`
}

type updater struct {
	mu   sync.Mutex
	path string
}

func newUpdater(dataDir string) *updater {
	return &updater{path: filepath.Join(dataDir, "update-job.json")}
}

func (u *updater) get() *updateJob {
	b, err := os.ReadFile(u.path)
	if err != nil {
		return nil
	}
	var j updateJob
	if json.Unmarshal(b, &j) != nil {
		return nil
	}
	return &j
}

func (u *updater) set(j *updateJob) {
	b, _ := json.Marshal(j)
	tmp := u.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, u.path)
	}
}

func (u *updater) busy() bool {
	j := u.get()
	if j == nil || j.Status != "running" {
		return false
	}
	// 下载线程异常退出（进程重启等）会留下永远 running 的记录，
	// 用开始时间兜底，超过 15 分钟就当它已经死了。
	if t, err := time.Parse("2006-01-02T15:04:05-07:00", j.StartedAt); err == nil {
		if time.Since(t) > 15*time.Minute {
			return false
		}
	}
	return true
}

func ghGet(rawURL string, out any) error {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "nm3u8dlre-ugos/1.0")
	req.Header.Set("Accept", "application/vnd.github+json")
	cl := &http.Client{Timeout: ghTimeout}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// 编译期注入（-X main.engineVersion=<上游 tag 去 v>），appVersion 以系统
// config.json 里的四段版本号为准 —— 用户要看得到构建号，否则"装了新版却没变"。
var (
	appVersion    = "dev"
	engineVersion = "dev"
)

// localVersion 返回应用版本与当前生效的引擎版本。
func (s *server) localVersion() (app, engine string) {
	app = appVersion
	if b, err := os.ReadFile(filepath.Join(s.paths.Install, "config.json")); err == nil {
		var cfg struct {
			Version struct {
				Version string `json:"version"`
			} `json:"version"`
		}
		if json.Unmarshal(b, &cfg) == nil && cfg.Version.Version != "" {
			app = cfg.Version.Version
		}
	}
	engine = engineVersion
	if b, err := os.ReadFile(filepath.Join(s.paths.Data, "engine", "engine-version")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "engineVersion="); ok && strings.TrimSpace(v) != "" {
				engine = strings.TrimSpace(v)
			}
		}
	}
	return
}

func jsonStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolPtr(b bool) any { return b }

// verGT 判断 a 是否比 b 新（支持 v 前缀与 -beta 这类预发布后缀）。
func verGT(a, b string) bool {
	a, b = strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v")
	if a == "" || b == "" || a == b {
		return false
	}
	pa, prea := splitPre(a)
	pb, preb := splitPre(b)
	na := strings.Split(pa, ".")
	nb := strings.Split(pb, ".")
	for i := 0; i < len(na) || i < len(nb); i++ {
		var x, y int
		if i < len(na) {
			x = atoiOr(na[i], 0)
		}
		if i < len(nb) {
			y = atoiOr(nb[i], 0)
		}
		if x != y {
			return x > y
		}
	}
	// 数字部分相同：没有预发布后缀的更新（1.0.0 > 1.0.0-beta）
	if prea == "" && preb != "" {
		return true
	}
	if prea != "" && preb == "" {
		return false
	}
	return prea > preb
}

func splitPre(v string) (string, string) {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

func archTag() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("不支持的 CPU 架构: %s", runtime.GOARCH)
}

// latestEngineRelease 取上游最新一条 Release（含预发布，与上游 fnOS 版口径一致）。
func latestEngineRelease() (*ghRelease, error) {
	var rels []ghRelease
	if err := ghGet("https://api.github.com/repos/"+engineRepo+"/releases?per_page=1", &rels); err != nil {
		return nil, err
	}
	if len(rels) == 0 {
		return nil, errors.New("上游暂无 Release")
	}
	return &rels[0], nil
}

func pickEngineAsset(rel *ghRelease, arch string) *ghAsset {
	for i := range rel.Assets {
		n := rel.Assets[i].Name
		if strings.HasPrefix(n, "N_m3u8DL-RE_") && strings.Contains(n, "_linux-"+arch+"_") && strings.HasSuffix(n, ".tar.gz") {
			return &rel.Assets[i]
		}
	}
	return nil
}

func (s *server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	app, engine := s.localVersion()
	resp := map[string]any{
		"ok":            true,
		"network":       "ok",
		"appVersion":    jsonStr(app),
		"engineVersion": jsonStr(engine),
		"engine": map[string]any{
			"latest": nil, "publishedAt": nil, "releaseUrl": nil,
			"upToDate": nil, "error": nil,
		},
		"app": map[string]any{
			"latest": nil, "publishedAt": nil, "releaseUrl": nil,
			"upToDate": nil, "asset": nil,
			"error": "应用本体请通过应用中心或应用商店更新（本页只负责升级下载引擎）",
		},
		"updateJob": s.upd.get(),
	}

	if s.skipNet() {
		resp["network"] = "skipped"
		writeJSON(w, 200, resp)
		return
	}

	rel, err := latestEngineRelease()
	eng := resp["engine"].(map[string]any)
	if err != nil {
		resp["network"] = "unreachable"
		eng["error"] = "网络不可达（无法访问 GitHub）: " + err.Error()
		writeJSON(w, 200, resp)
		return
	}
	eng["latest"] = jsonStr(rel.TagName)
	eng["publishedAt"] = jsonStr(rel.PublishedAt)
	eng["releaseUrl"] = jsonStr(rel.HTMLURL)
	if engine != "" && rel.TagName != "" {
		eng["upToDate"] = boolPtr(!verGT(rel.TagName, engine))
	}
	writeJSON(w, 200, resp)
}

// skipNet 让离线环境（或测试）跳过联网检查。
func (s *server) skipNet() bool { return os.Getenv("NRE_UPDATE_SKIP_NET") == "1" }

func (s *server) handleEngineUpdate(w http.ResponseWriter, r *http.Request) {
	if s.upd.busy() {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "已有更新任务进行中，请稍候"})
		return
	}
	if s.skipNet() {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "当前环境已禁用联网（测试模式）"})
		return
	}
	go s.runEngineUpdate()
	writeJSON(w, 200, map[string]any{"ok": true, "started": true})
}

func (s *server) runEngineUpdate() {
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()

	job := &updateJob{Mode: "engine", Status: "running", StartedAt: nowISO(), Message: "正在查询最新版本…"}
	s.upd.set(job)
	fail := func(format string, a ...any) {
		job.Status = "failed"
		job.Message = fmt.Sprintf(format, a...)
		job.FinishedAt = nowISO()
		s.upd.set(job)
	}

	arch, err := archTag()
	if err != nil {
		fail("%v", err)
		return
	}
	rel, relErr := latestEngineRelease()
	if relErr != nil {
		fail("查询 GitHub 失败：%v", relErr)
		return
	}
	asset := pickEngineAsset(rel, arch)
	if asset == nil {
		fail("未找到 %s 架构的发布包", arch)
		return
	}
	job.TargetVersion = strings.TrimPrefix(rel.TagName, "v")
	job.Name = asset.Name
	job.Size = asset.Size
	job.Message = "正在下载引擎…"
	s.upd.set(job)

	dir := filepath.Join(s.paths.Data, "engine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail("无法创建引擎目录：%v", err)
		return
	}
	tmpTar := filepath.Join(dir, ".download.tar.gz")
	defer os.Remove(tmpTar)

	// 用 API 资产端点下载，避免浏览器直链被墙/被限速
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/assets/%d", engineRepo, asset.ID)
	if err := downloadFile(url, tmpTar, true); err != nil {
		fail("下载失败：%v", err)
		return
	}
	job.Message = "正在解包校验…"
	s.upd.set(job)

	tmpBin := filepath.Join(dir, ".N_m3u8DL-RE.tmp")
	if err := extractBinary(tmpTar, "N_m3u8DL-RE", tmpBin); err != nil {
		fail("解包失败：%v", err)
		return
	}
	if err := os.Chmod(tmpBin, 0o755); err != nil {
		fail("设置执行权限失败：%v", err)
		return
	}
	target := filepath.Join(dir, "N_m3u8DL-RE")
	_ = os.Remove(target)
	if err := os.Rename(tmpBin, target); err != nil {
		fail("替换引擎失败：%v", err)
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "engine-version"), []byte("engineVersion="+job.TargetVersion+"\n"), 0o644)

	job.Status = "ok"
	job.Message = "核心引擎已更新到 " + job.TargetVersion + "（下一个任务生效）"
	job.FinishedAt = nowISO()
	job.Path = target
	s.upd.set(job)
}

func downloadFile(rawURL, dest string, useOctet bool) error {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "nm3u8dlre-ugos/1.0")
	if useOctet {
		req.Header.Set("Accept", "application/octet-stream")
	}
	// 有总超时：GitHub 的资产 CDN 在国内经常只吐几百 KB 就断流，
	// 不设超时会让 updateJob 永远停在 running，把后续更新全挡在 busy() 外面。
	cl := &http.Client{Timeout: 10 * time.Minute}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// extractBinary 从 tar.gz 中取出指定文件名（取第一个匹配）。
func extractBinary(tarPath, name, dest string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != name {
			continue
		}
		out, err := os.Create(dest)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		out.Close()
		return err
	}
	return fmt.Errorf("压缩包里没有 %s", name)
}

var _ = strconv.Itoa
