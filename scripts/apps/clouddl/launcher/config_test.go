package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 造一份"看起来像装好了"的目录布局。
func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	p := Paths{
		Install:         filepath.Join(root, "install"),
		Data:            filepath.Join(root, "data"),
		Cache:           filepath.Join(root, "cache"),
		Log:             filepath.Join(root, "log"),
		Config:          filepath.Join(root, "data", "config"),
		Tmp:             filepath.Join(root, "cache", "tmp"),
		DownloadDirFile: filepath.Join(root, "data", "download_dir"),
		LauncherConfig:  filepath.Join(root, "data", "launcher.json"),
	}
	if err := p.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	return p
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// 首次启动：没有 download_dir 文件，参数说了算。
func TestResolveDownloadDir_FirstBootUsesParameter(t *testing.T) {
	p := testPaths(t)
	dir := mkdir(t, filepath.Join(t.TempDir(), "网盘下载"))
	t.Setenv("CLOUDDL_DOWNLOAD_DIR", dir)

	cfg := loadLauncherConfig(p.LauncherConfig)
	got := resolveDownloadDir(p, &cfg)

	if got.Dir != dir {
		t.Fatalf("应该采纳参数 %q，实际 %q", dir, got.Dir)
	}
	if got.Fallback {
		t.Fatal("参数有效时不该退到兜底目录")
	}
	// 结果要同步给上游，否则上游会拿它自己的默认值去建目录
	if readDownloadDirFile(p.DownloadDirFile) != dir {
		t.Fatal("没有把选定目录写进 download_dir 文件")
	}
}

// 平时启动：应用内选的目录优先，【绝不跟着参数漂】。
//
// 这条是这个文件里最要紧的一条：上游允许用户在「设置 - 下载策略」里改目录，
// 每次启动都跟参数走的话，用户第二天重启 NAS 就会发现文件又下到别处去了。
func TestResolveDownloadDir_DoesNotDriftBackToParameter(t *testing.T) {
	p := testPaths(t)
	param := mkdir(t, filepath.Join(t.TempDir(), "参数目录"))
	inApp := mkdir(t, filepath.Join(t.TempDir(), "用户在应用里改的"))

	// 上一次启动时参数就是这个值
	if err := saveLauncherConfig(p.LauncherConfig, launcherConfig{LastParamDir: param}); err != nil {
		t.Fatal(err)
	}
	// 用户之后在应用里把目录改成了别的
	if err := writeDownloadDirFile(p.DownloadDirFile, inApp); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDDL_DOWNLOAD_DIR", param)

	cfg := loadLauncherConfig(p.LauncherConfig)
	got := resolveDownloadDir(p, &cfg)

	if got.Dir != inApp {
		t.Fatalf("应该保持应用内选的 %q，实际漂回了 %q", inApp, got.Dir)
	}
}

// 用户在应用中心把参数改了 —— 这一次要跟着参数走。
func TestResolveDownloadDir_ParameterChangeWins(t *testing.T) {
	p := testPaths(t)
	old := mkdir(t, filepath.Join(t.TempDir(), "旧"))
	neu := mkdir(t, filepath.Join(t.TempDir(), "新"))

	if err := saveLauncherConfig(p.LauncherConfig, launcherConfig{LastParamDir: old}); err != nil {
		t.Fatal(err)
	}
	if err := writeDownloadDirFile(p.DownloadDirFile, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDDL_DOWNLOAD_DIR", neu)

	cfg := loadLauncherConfig(p.LauncherConfig)
	got := resolveDownloadDir(p, &cfg)

	if got.Dir != neu {
		t.Fatalf("参数改成 %q 后应该采纳它，实际 %q", neu, got.Dir)
	}
	if got.Note == "" {
		t.Fatal("换了目录这么大的事，得有一句给用户看的说明")
	}
	// 记账要落盘，否则下次启动会把"没变化"误判成"又改了一次"
	if loadLauncherConfig(p.LauncherConfig).LastParamDir != neu {
		t.Fatal("新的参数值没有记下来")
	}
}

// 全新安装后的第一次启动：参数是空的、授权目录还没挂进来。
// 这是【正常】状态，必须照常起来，不能把应用卡死在这。
func TestResolveDownloadDir_EmptyOnFirstBootFallsBack(t *testing.T) {
	p := testPaths(t)
	t.Setenv("CLOUDDL_DOWNLOAD_DIR", "")

	cfg := loadLauncherConfig(p.LauncherConfig)
	got := resolveDownloadDir(p, &cfg)

	if !got.Fallback {
		t.Fatal("参数为空时应该退到应用私有目录")
	}
	if got.Dir != filepath.Join(p.Data, "downloads") {
		t.Fatalf("兜底目录不对：%q", got.Dir)
	}
	if _, err := os.Stat(got.Dir); err != nil {
		t.Fatalf("兜底目录得真的建出来：%v", err)
	}
	if got.Note == "" {
		t.Fatal("要留一句说明，否则用户找不到文件下哪去了")
	}
}

// 一个都没选时平台写的是字面量 null，不是空字符串。
func TestParamDownloadDir_TreatsNullAsEmpty(t *testing.T) {
	t.Setenv("CLOUDDL_DOWNLOAD_DIR", "null")
	if got := paramDownloadDir(); got != "" {
		t.Fatalf("字面量 null 必须当成没选，实际 %q", got)
	}
	// 相对路径同样不可信（平台不会给，但别让它一路传到 MkdirAll）
	t.Setenv("CLOUDDL_DOWNLOAD_DIR", "downloads")
	if got := paramDownloadDir(); got != "" {
		t.Fatalf("相对路径必须拒掉，实际 %q", got)
	}
}

// 目录存在但写不进去（首启授权还没生效就是这个样子）：退兜底并说清楚。
func TestResolveDownloadDir_UnwritableFallsBackWithNote(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 对谁都可写，这条测不了")
	}
	p := testPaths(t)
	dir := mkdir(t, filepath.Join(t.TempDir(), "只读"))
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDDL_DOWNLOAD_DIR", dir)

	cfg := loadLauncherConfig(p.LauncherConfig)
	got := resolveDownloadDir(p, &cfg)

	if !got.Fallback {
		t.Fatal("写不进去的目录不能拿来用")
	}
	if got.Note == "" {
		t.Fatal("必须告诉用户为什么没用他选的目录")
	}
}

// 默认设置只在文件不存在时写，绝不覆盖用户已有的设置。
func TestEnsureDefaultSettings(t *testing.T) {
	dir := t.TempDir()
	if err := ensureDefaultSettings(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" {
		t.Fatal("默认设置是空的")
	}

	// 用户改过之后再启动一次，内容必须原样保留
	custom := []byte(`{"aria2_enabled": true}`)
	if err := os.WriteFile(path, custom, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureDefaultSettings(dir); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != string(custom) {
		t.Fatalf("已有设置被覆盖了：%s", again)
	}
}
