package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkfile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 一个目录都没配 —— 首次启动必然是这样，提示必须点名"停止再启动一次"，
// 因为那才是用户真正要做的动作。
func TestDiagnoseNoRoots(t *testing.T) {
	got := diagnoseRoots(nil)
	if len(got) != 1 {
		t.Fatalf("期望一条提示，实际 %v", got)
	}
	for _, kw := range []string{"停止", "启动", "设置"} {
		if !strings.Contains(got[0], kw) {
			t.Errorf("提示里没有 %q，用户不知道该做什么：%s", kw, got[0])
		}
	}
}

// 目录不存在 = 首启授权还没生效，这是最常见的一种，提示要指向重启而不是"目录没了"。
func TestDiagnoseMissingRoot(t *testing.T) {
	got := diagnoseRoots([]string{filepath.Join(t.TempDir(), "不存在")})
	if len(got) != 1 || !strings.Contains(got[0], "不存在") {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(got[0], "停止") {
		t.Errorf("没提示重启：%s", got[0])
	}
}

// 有音频文件 —— 正常路径，别报警。
func TestDiagnoseHasAudio(t *testing.T) {
	root := t.TempDir()
	mkfile(t, filepath.Join(root, "专辑 A", "01.flac"))
	mkfile(t, filepath.Join(root, "专辑 A", "02.mp3"))
	mkfile(t, filepath.Join(root, "封面.jpg"))

	got := diagnoseRoots([]string{root})
	if len(got) != 1 {
		t.Fatalf("%v", got)
	}
	if strings.HasPrefix(got[0], "⚠") {
		t.Errorf("正常情况不该报警：%s", got[0])
	}
	if !strings.Contains(got[0], "2 个音频文件") {
		t.Errorf("数错了：%s", got[0])
	}
}

// 空目录 / 只有不支持的格式 —— 要把支持的格式列出来，否则用户无从判断。
func TestDiagnoseNoAudio(t *testing.T) {
	root := t.TempDir()
	mkfile(t, filepath.Join(root, "说明.txt"))
	got := diagnoseRoots([]string{root})
	if !strings.Contains(got[0], "flac") {
		t.Errorf("没列出支持的格式：%s", got[0])
	}
}

// 只有子目录、没有直接的音频文件时，提示"可能是层级选错了" —— 这是
// course-manager 上真实发生过的报障（用户把库选高了一层）。
func TestDiagnoseOnlyDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "备份"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := diagnoseRoots([]string{root})
	if !strings.Contains(got[0], "子文件夹") {
		t.Errorf("没提到层级问题：%s", got[0])
	}
}

// 点开头的目录要跳过（@eaDir、.materials 之类），别把它们的内容算进来。
func TestProbeSkipsHiddenDirs(t *testing.T) {
	root := t.TempDir()
	mkfile(t, filepath.Join(root, ".thumbnails", "a.mp3"))
	mkfile(t, filepath.Join(root, "真的.mp3"))
	if d := probeRoot(root); d.Audio != 1 {
		t.Errorf("Audio = %d，期望 1（隐藏目录里的不该算）", d.Audio)
	}
}

// 大库上要提前收手，不能在启动路径上把整个音乐库走一遍。
func TestProbeStopsEarly(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < probeStopAt+30; i++ {
		mkfile(t, filepath.Join(root, "t"+itoa(i)+".mp3"))
	}
	d := probeRoot(root)
	if d.Audio != probeStopAt || !d.Partial {
		t.Errorf("Audio=%d Partial=%v，期望数到 %d 就停", d.Audio, d.Partial, probeStopAt)
	}
	if got := diagnoseRoots([]string{root}); !strings.Contains(got[0], "+") {
		t.Errorf("提前停下时应当显示成 50+：%s", got[0])
	}
}

// 诊断读的是【配置文件里最终的 roots】，用户在应用里自己加的目录也要覆盖到。
func TestConfiguredRootsReadsBackWhatWeWrote(t *testing.T) {
	p := testPaths(t)
	if _, _, err := syncConfig(p, 28085, Params{MusicPaths: []string{"/volume1/a"}}); err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(readConfig(t, p), `roots = ["/volume1/a"]`,
		`roots = ["/volume1/a", "/volume1/用户自己加的"]`, 1)
	if err := os.WriteFile(p.ConfigFile(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got := configuredRoots(p)
	if len(got) != 2 || got[1] != "/volume1/用户自己加的" {
		t.Errorf("configuredRoots = %v", got)
	}
}

func TestItoa(t *testing.T) {
	for _, c := range []struct {
		in   int
		want string
	}{{0, "0"}, {7, "7"}, {50, "50"}, {1234, "1234"}} {
		if got := itoa(c.in); got != c.want {
			t.Errorf("itoa(%d) = %q", c.in, got)
		}
	}
}
