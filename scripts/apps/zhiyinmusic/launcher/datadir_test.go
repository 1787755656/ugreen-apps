package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testDataPaths 造一份"平台数据目录 + 缓存目录"都在临时目录里的 Paths。
func testDataPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	p := Paths{
		Root:         filepath.Join(root, "install"),
		Bin:          filepath.Join(root, "install", "bin"),
		Lib:          filepath.Join(root, "install", "lib"),
		App:          filepath.Join(root, "install", "app"),
		Data:         filepath.Join(root, "appdata"),
		PlatformData: filepath.Join(root, "appdata"),
		Cache:        filepath.Join(root, "appcache"),
	}
	for _, d := range []string{p.Root, p.Data, p.Cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 没选配置文件夹 → 就用平台数据目录，并且把锚点写下来（用户之后可能才选）。
func TestUseDataDirDefaultsToPlatformDir(t *testing.T) {
	p := testDataPaths(t)
	notes := p.useDataDir(Params{})

	if p.Data != p.PlatformData {
		t.Errorf("Data = %s，应当还是平台数据目录 %s", p.Data, p.PlatformData)
	}
	if got := readDataDirState(p.PlatformData); got != p.PlatformData {
		t.Errorf("状态文件 = %q，应当是 %q", got, p.PlatformData)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "没有指定") {
		t.Errorf("日志里应当说明用的是默认数据目录：%v", notes)
	}
}

// 选了配置文件夹 → 切过去，旧位置一个文件都不许丢。
func TestUseDataDirMigratesData(t *testing.T) {
	p := testDataPaths(t)
	writeFile(t, filepath.Join(p.Data, "config.toml"), "x = 1\n")
	writeFile(t, filepath.Join(p.Data, "db.sqlite"), "SQLite format 3\x00")
	writeFile(t, filepath.Join(p.Data, "covers", "a", "1.jpg"), "jpg")
	writeFile(t, filepath.Join(p.Data, ".zhiyin", "key_pepper"), "pepper")
	writeFile(t, filepath.Join(p.Data, "home", ".cache"), "")

	want := filepath.Join(t.TempDir(), "zhiyin")
	p.useDataDir(Params{DataDir: want})

	if p.Data != want {
		t.Fatalf("Data = %s，应当切到 %s", p.Data, want)
	}
	for _, rel := range []string{"config.toml", "db.sqlite", "covers/a/1.jpg", ".zhiyin/key_pepper", "home/.cache"} {
		if _, err := os.Stat(filepath.Join(want, rel)); err != nil {
			t.Errorf("没搬过去：%s（%v）", rel, err)
		}
	}
	// 旧位置必须被腾空：config.toml / db.sqlite 都不在原地了，但数据还在 .migrated-* 里。
	if hasActiveData(p.PlatformData) {
		t.Error("旧位置还留着 config.toml/db.sqlite —— 之后把参数改回来会静默回滚到过期数据")
	}
	migrated, _ := filepath.Glob(filepath.Join(p.PlatformData, ".migrated-*", "db.sqlite"))
	if len(migrated) != 1 {
		t.Errorf("旧数据应当被挪进 .migrated-*/\u91cc，实际 %v", migrated)
	}
	if got := readDataDirState(p.PlatformData); got != want {
		t.Errorf("状态文件 = %q，应当是 %q", got, want)
	}
}

// 第一次启动就选了文件夹（数据目录还是空的）→ 建目录即可，不用报错。
func TestUseDataDirFreshInstall(t *testing.T) {
	p := testDataPaths(t)
	want := filepath.Join(t.TempDir(), "zhiyin")
	notes := p.useDataDir(Params{DataDir: want})
	if p.Data != want {
		t.Fatalf("Data = %s", p.Data)
	}
	if !isDir(want) {
		t.Error("新目录没建出来")
	}
	if strings.Contains(strings.Join(notes, "\n"), "⚠") {
		t.Errorf("全新安装不该有警告：%v", notes)
	}
}

// 新目录里已经有数据（用户指回一份备份）→ 以它为准，绝不覆盖。
func TestUseDataDirKeepsExistingTargetData(t *testing.T) {
	p := testDataPaths(t)
	writeFile(t, filepath.Join(p.Data, "db.sqlite"), "old")
	want := filepath.Join(t.TempDir(), "zhiyin")
	writeFile(t, filepath.Join(want, "db.sqlite"), "backup")

	p.useDataDir(Params{DataDir: want})

	b, err := os.ReadFile(filepath.Join(want, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "backup" {
		t.Errorf("新目录里的数据被覆盖了：%q", string(b))
	}
	if hasActiveData(p.PlatformData) {
		t.Error("旧位置应当被腾空")
	}
}

// 参数不合法（相对路径 / 根目录 / 存储空间根 / 安装目录里）→ 继续用平台目录，不报错。
func TestUseDataDirRejectsBadPaths(t *testing.T) {
	for _, bad := range []string{"relative/path", "/", "/volume1"} {
		p := testDataPaths(t)
		notes := p.useDataDir(Params{DataDir: bad})
		if p.Data != p.PlatformData {
			t.Errorf("%q 不该被采用，Data = %s", bad, p.Data)
		}
		if !strings.Contains(strings.Join(notes, "\n"), "⚠") {
			t.Errorf("%q 应当留一条警告：%v", bad, notes)
		}
	}

	p := testDataPaths(t)
	inside := filepath.Join(p.Root, "data")
	notes := p.useDataDir(Params{DataDir: inside})
	if p.Data != p.PlatformData {
		t.Errorf("安装目录里是只读的，不该被采用：%s", p.Data)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "只读") {
		t.Errorf("应当说明原因：%v", notes)
	}
}

// 目录写不进去（首启授权没生效就是这个症状）→ 原地不动，给一句能照做的话。
func TestUseDataDirUnwritableFallsBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限位")
	}
	p := testDataPaths(t)
	writeFile(t, filepath.Join(p.Data, "config.toml"), "x = 1\n")

	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.MkdirAll(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blocked, 0o755) // 让 t.TempDir 的清理能跑

	notes := p.useDataDir(Params{DataDir: filepath.Join(blocked, "zhiyin")})
	if p.Data != p.PlatformData {
		t.Errorf("写不进去时应当留在原地，实际 %s", p.Data)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "授权") {
		t.Errorf("应当提示授权还没生效：%v", notes)
	}
	if _, err := os.Stat(filepath.Join(p.Data, "config.toml")); err != nil {
		t.Error("原地不动时数据必须还在")
	}
}

// 配置文件夹和音乐目录互相嵌套 → 必须点名警告：平台在父子同时被选中时不授权父目录。
func TestUseDataDirNestingWarning(t *testing.T) {
	p := testDataPaths(t)
	music := filepath.Join(t.TempDir(), "music")
	// 连跑两次：第一次落库、第二次才走"参数和当前一致"的分支 —— 两条路都得给警告。
	p.useDataDir(Params{DataDir: filepath.Join(music, "zhiyin"), MusicPaths: []string{music}})
	notes := p.useDataDir(Params{DataDir: filepath.Join(music, "zhiyin"), MusicPaths: []string{music}})
	if !strings.Contains(strings.Join(notes, "\n"), "不会授权父目录") {
		t.Errorf("应当警告嵌套：%v", notes)
	}
}

func TestParseScalarPath(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"裸标量", "/volume1/docker/zhiyin", "/volume1/docker/zhiyin"},
		{"带引号", `'/volume1/docker/zhiyin'`, "/volume1/docker/zhiyin"},
		{"尾部斜杠", "/volume1/docker/zhiyin/", "/volume1/docker/zhiyin"},
		// multi 从 true 改成 false 时的串味写法
		{"数组取第一个", `["/volume1/docker/zhiyin"]`, "/volume1/docker/zhiyin"},
		{"没选", "", ""},
		{"字面量 null", "null", ""},
		{"相对路径", "docker/zhiyin", ""},
		{"垃圾", "garbage", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseScalarPath(c.raw); got != c.want {
				t.Errorf("parseScalarPath(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestReadParamsDataDir(t *testing.T) {
	env := map[string]string{envDataDir: "/volume1/docker/zhiyin"}
	p := readParams(func(k string) string { return env[k] })
	if p.DataDir != "/volume1/docker/zhiyin" {
		t.Errorf("DataDir = %q", p.DataDir)
	}
}
